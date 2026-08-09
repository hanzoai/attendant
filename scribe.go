// The meeting's record: a floor's turns, transcribed by hanzoai/speech.
//
// One turn is one growing transcript there — open, push, close. Because turns
// never overlap, the service holds one session per meeting at a time, which is
// the whole economy of the floor: a room of any size costs one decode.
//
// Pushing is done by a goroutine per turn rather than by the caller. The floor
// runs on the room's clock and cannot wait on a POST; a service under load must
// slow the record, never the audio. When the queue is nonetheless full the frame
// is dropped and counted, because audio silently discarded is a transcript that
// reads complete and is not.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	// What one push carries, and the most one may: hanzoai/speech refuses a
	// larger body rather than truncating it (transcript.CHUNK, transcript.CEILING).
	chunk = 8 * 1024

	// Audio one of its sessions accepts before it answers 409 (transcript.LIMIT).
	// A turn crossing this is rolled into a fresh session, so a keynote is
	// transcribed to its end.
	span = 600 * time.Second

	// Frames a turn will hold while the service catches up. Two seconds of
	// audio: long enough to ride out a slow push, short enough that a stalled
	// service is visible as loss rather than as growing memory.
	queue = 100
)

// Said is one turn as recorded: who spoke, when, and what the service made of it.
type Said struct {
	Speaker string
	At      time.Time
	Text    string
	Seconds float64
}

// Notes is the record of one meeting.
type Notes struct {
	url   string // hanzoai/speech base URL
	model string
	lang  string
	http  *http.Client

	mu   sync.Mutex
	said map[int]Said
	next int
	lost int
}

// Record opens a record kept by the hanzoai/speech at url.
func Record(url, model, lang string) *Notes {
	return &Notes{
		url:   url,
		model: model,
		lang:  lang,
		http:  &http.Client{Timeout: 30 * time.Second},
		said:  map[int]Said{},
	}
}

// Turn opens a transcript for speaker.
func (n *Notes) Turn(speaker string, at time.Time) (Turn, error) {
	id, err := n.open()
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	seq := n.next
	n.next++
	n.mu.Unlock()

	t := &turn{to: n, seq: seq, speaker: speaker, at: at, id: id,
		in: make(chan []int16, queue), gone: make(chan struct{})}
	go t.push()
	return t, nil
}

// Said is every turn closed so far, in the order the turns began. Turns close
// out of order — a long one settles after the short one that followed it — so
// they are ordered by when they opened, which is when they were said.
func (n *Notes) Said() []Said {
	n.mu.Lock()
	defer n.mu.Unlock()
	seqs := make([]int, 0, len(n.said))
	for s := range n.said {
		seqs = append(seqs, s)
	}
	sort.Ints(seqs)
	out := make([]Said, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, n.said[s])
	}
	return out
}

// Lost is frames dropped because the service could not keep up.
func (n *Notes) Lost() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lost
}

// Text is the meeting, attributed. One line per turn, which is one line per
// time the floor changed hands.
func (n *Notes) Text() string {
	var b bytes.Buffer
	for _, s := range n.Said() {
		if s.Text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", s.Speaker, s.Text)
	}
	return b.String()
}

// turn is one open transcript.
type turn struct {
	to      *Notes
	seq     int
	speaker string
	at      time.Time

	in   chan []int16
	gone chan struct{}
	once sync.Once

	// Owned by push().
	id      string
	pending []byte
	sent    float64 // seconds pushed into the current session
	text    string  // settled text of sessions already rolled over
	total   float64
	err     error
}

// Hear queues a frame. Never blocks: a full queue means the service is behind,
// and the room cannot be made to wait for it.
func (t *turn) Hear(pcm []int16) {
	select {
	case t.in <- append([]int16(nil), pcm...):
	default:
		t.to.mu.Lock()
		t.to.lost++
		t.to.mu.Unlock()
	}
}

// Close ends the turn: the queue drains, the last part-chunk is pushed, and the
// service settles the text. Blocks, which is why the floor closes turns off the
// audio path.
func (t *turn) Close() error {
	t.once.Do(func() { close(t.in) })
	<-t.gone
	return t.err
}

// push is the turn's only writer. It owns the session and everything derived
// from it, so nothing here needs a lock.
func (t *turn) push() {
	defer close(t.gone)
	for pcm := range t.in {
		for _, s := range pcm {
			t.pending = binary.LittleEndian.AppendUint16(t.pending, uint16(s))
		}
		for len(t.pending) >= chunk {
			t.send(t.pending[:chunk])
			t.pending = t.pending[chunk:]
		}
	}
	if len(t.pending) > 0 {
		t.send(t.pending)
		t.pending = nil
	}
	t.settle()
}

// send pushes one chunk, rolling over to a new session first if this one is at
// the service's limit.
func (t *turn) send(pcm []byte) {
	if t.sent+float64(len(pcm))/(rate*2) > span.Seconds() {
		t.roll()
	}
	if t.err != nil {
		return
	}
	if _, err := t.post(t.url(t.id), bytes.NewReader(pcm), "application/octet-stream"); err != nil {
		t.err = err
		return
	}
	t.sent += float64(len(pcm)) / (rate * 2)
}

// roll closes the session at its limit and opens the next, keeping what the
// closed one settled. The turn continues; only the session behind it changes.
func (t *turn) roll() {
	said, err := t.close(t.id)
	if err != nil {
		t.err = err
		return
	}
	t.text = join(t.text, said.Text)
	t.total += said.Seconds

	id, err := t.to.open()
	if err != nil {
		t.err = err
		return
	}
	t.id, t.sent = id, 0
}

// settle closes the session and files the turn.
func (t *turn) settle() {
	said, err := t.close(t.id)
	if err != nil {
		if t.err == nil {
			t.err = err
		}
		return
	}
	t.to.mu.Lock()
	t.to.said[t.seq] = Said{
		Speaker: t.speaker,
		At:      t.at,
		Text:    join(t.text, said.Text),
		Seconds: t.total + said.Seconds,
	}
	t.to.mu.Unlock()
}

func (t *turn) url(id string) string { return t.to.url + "/v1/audio/transcript/" + id }

// state is what a push or a close answers with.
type state struct {
	ID      string  `json:"id"`
	Text    string  `json:"text"`
	Pending string  `json:"pending"`
	Seconds float64 `json:"seconds"`
}

// open begins a session. The shape is stated in full because the service checks
// it and refuses a mismatch rather than resampling: audio carries no header, so
// a wrong rate is gibberish, not an error.
func (n *Notes) open() (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": n.model, "language": n.lang,
		"format": "pcm16", "rate": rate, "channels": channels,
	})
	got, err := n.post(n.url+"/v1/audio/transcript", bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	return got.ID, nil
}

func (t *turn) close(id string) (state, error) {
	req, err := http.NewRequest(http.MethodDelete, t.url(id), nil)
	if err != nil {
		return state{}, err
	}
	return t.to.do(req)
}

func (t *turn) post(url string, body io.Reader, mime string) (state, error) {
	return t.to.post(url, body, mime)
}

func (n *Notes) post(url string, body io.Reader, mime string) (state, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return state{}, err
	}
	req.Header.Set("Content-Type", mime)
	return n.do(req)
}

func (n *Notes) do(req *http.Request) (state, error) {
	res, err := n.http.Do(req)
	if err != nil {
		return state{}, fmt.Errorf("speech %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()
	read, err := io.ReadAll(res.Body)
	if err != nil {
		return state{}, err
	}
	if res.StatusCode >= 300 {
		return state{}, fmt.Errorf("speech %s %s: %s: %s", req.Method, req.URL.Path, res.Status, bytes.TrimSpace(read))
	}
	var got state
	if err := json.Unmarshal(read, &got); err != nil {
		return state{}, fmt.Errorf("speech %s %s: %w", req.Method, req.URL.Path, err)
	}
	return got, nil
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}
