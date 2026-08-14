package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// service is the estate the attendant talks to, answering the way it answers:
// hanzoai/speech's transcript API — a session per open, a limit past which a
// push is refused, and the text on close — and beside it the two routes an
// answer is made of, the model and the voice.
type service struct {
	mu      sync.Mutex
	next    int
	got     map[string][]int // bytes of each push, per open session
	kept    map[string][]int // and per session already closed
	refused int              // pushes answered 409
	hold    chan struct{}    // when non-nil, pushes wait on it
	limit   float64          // seconds a session accepts before 409, 0 for no limit
	at      string           // where an open says its session lives, empty for here
	sent    []string         // the host each push was addressed to
	reply   string           // what the model answers
	asked   []string         // and what it was asked, last message first
}

func serve(t *testing.T, limit float64) (*service, string) {
	t.Helper()
	s := &service{got: map[string][]int{}, kept: map[string][]int{}, limit: limit}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *service) handle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/audio/transcript")
	id = strings.TrimPrefix(id, "/")

	switch {
	// The model, and the voice. One service stands in for the whole estate
	// because the attendant reaches all of it the same way.
	case r.URL.Path == "/v1/chat/completions":
		var heard struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&heard); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		if n := len(heard.Messages); n > 0 {
			s.asked = append(s.asked, heard.Messages[n-1].Content)
		}
		said := s.reply
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": said}}},
		})

	// Real kokoro output, so what the attendant publishes is the audio a room
	// would actually be given rather than a stand-in that decodes to nothing.
	case r.URL.Path == "/v1/audio/speech":
		var want struct {
			Format string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&want); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// The service refuses a format it cannot make rather than answering in
		// another one, so asking for the wrong container is an error and not an
		// MP3 in an Opus track, which a room carries as nothing at all.
		if want.Format != "opus" {
			http.Error(w, "unsupported response_format "+want.Format, 400)
			return
		}
		ogg, err := os.ReadFile("spoken.opus")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		w.Write(ogg)

	case r.Method == http.MethodPost && id == "":
		var open map[string]any
		if err := json.NewDecoder(r.Body).Decode(&open); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if open["format"] != "pcm16" || open["rate"] != float64(rate) || open["channels"] != float64(1) {
			http.Error(w, fmt.Sprintf("wrong shape: %v", open), 400)
			return
		}
		s.mu.Lock()
		s.next++
		id = fmt.Sprintf("atr_%d", s.next)
		s.got[id] = nil
		at := s.at
		s.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(state{ID: id, At: at})

	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.sent = append(s.sent, r.Host)
		hold, seen := s.hold, s.got[id]
		if seen == nil {
			if _, live := s.got[id]; !live {
				s.mu.Unlock()
				http.Error(w, "no transcript "+id, 404)
				return
			}
		}
		var sent int
		for _, n := range seen {
			sent += n
		}
		s.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if s.limit > 0 && float64(sent+len(body))/(rate*2) > s.limit {
			s.mu.Lock()
			s.refused++
			s.mu.Unlock()
			http.Error(w, "at its limit", 409)
			return
		}
		s.mu.Lock()
		s.got[id] = append(s.got[id], len(body))
		s.mu.Unlock()
		json.NewEncoder(w).Encode(state{ID: id})

	case r.Method == http.MethodDelete:
		s.mu.Lock()
		s.sent = append(s.sent, r.Host)
		pushes, live := s.got[id]
		if !live {
			s.mu.Unlock()
			http.Error(w, "no transcript "+id, 404)
			return
		}
		var sent int
		for _, n := range pushes {
			sent += n
		}
		s.kept[id] = pushes
		delete(s.got, id)
		s.mu.Unlock()
		json.NewEncoder(w).Encode(state{ID: id, Text: id, Seconds: float64(sent) / (rate * 2)})

	default:
		http.Error(w, "no", 405)
	}
}

func (s *service) pushes(id string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if open, live := s.got[id]; live {
		return append([]int(nil), open...)
	}
	return append([]int(nil), s.kept[id]...)
}

// says is what the model will answer. Set through the service's own lock, like
// everything else it holds, so the race detector has the whole picture.
func (s *service) says(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = text
}

// asks is what the model was asked, one entry per ask.
func (s *service) asks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// lives is where an open will say its session lives.
func (s *service) lives(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = url
}

// addressed is the host each push and close was sent to.
func (s *service) addressed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// taken is the audio the service has actually accepted, in seconds.
func (s *service) taken() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := 0
	for _, all := range []map[string][]int{s.got, s.kept} {
		for _, pushes := range all {
			for _, n := range pushes {
				sent += n
			}
		}
	}
	return float64(sent) / (rate * 2)
}

// The service refuses a body larger than its ceiling and one that is not whole
// int16 frames, rather than truncating either. A turn that pushes the wrong
// shape gets a 400 for every chunk and transcribes nothing.
func TestAPushIsWholeFramesInsideTheCeiling(t *testing.T) {
	s, url := serve(t, 0)
	notes := Record(url, "whisper", "en")

	turn, err := notes.Turn("ana", time.Now())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Two and a half chunks, so both the full pushes and the remainder are seen.
	for i := 0; i < 5; i++ {
		turn.Hear(make([]int16, chunk/2/2))
	}
	if err := turn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sent := s.pushes("atr_1")
	if len(sent) != 3 {
		t.Fatalf("%d pushes for 2.5 chunks of audio: %v", len(sent), sent)
	}
	for i, n := range sent {
		if n%2 != 0 {
			t.Fatalf("push %d is %d bytes, not whole int16 frames", i, n)
		}
		if n > chunk {
			t.Fatalf("push %d is %d bytes, over the %d ceiling", i, n, chunk)
		}
	}
	if sent[0] != chunk || sent[1] != chunk || sent[2] != chunk/2 {
		t.Fatalf("pushes %v; wanted two full chunks and the remainder", sent)
	}
}

// A session takes 600 s of audio and then answers 409. A keynote is longer than
// that, so the turn rolls into a fresh session and keeps what the last one
// settled — otherwise the second half of a long turn is transcribed by nobody
// and every push after the limit is an error nobody reads.
func TestALongTurnRollsOverRatherThanBeingRefused(t *testing.T) {
	s, url := serve(t, span.Seconds())
	notes := Record(url, "whisper", "en")

	turn, err := notes.Turn("ana", time.Now())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Ten seconds past the limit, a second at a time, never more than a minute
	// ahead of the service — which is what a room does, since audio arrives on a
	// clock and cannot arrive faster than it is spoken.
	const over = 10
	for fed := 0.0; fed < span.Seconds()+over; fed++ {
		turn.Hear(make([]int16, rate))
		for s.taken() < fed-60 {
			time.Sleep(time.Millisecond)
		}
	}
	if err := turn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	said := notes.Said()
	if len(said) != 1 {
		t.Fatalf("%d turns; a rollover is one turn, not two", len(said))
	}
	if s.next < 2 {
		t.Fatalf("%d sessions opened; %v of audio does not fit in one", s.next, span)
	}
	// Every second submitted is accounted for, across both sessions.
	if s.refused > 0 {
		t.Fatalf("%d pushes answered 409; the turn rolled over too late", s.refused)
	}
	if notes.Lost() > 0 {
		t.Fatalf("%d frames dropped feeding the turn", notes.Lost())
	}
	if got := said[0].Seconds; got < span.Seconds()+over-1 {
		t.Fatalf("turn reports %.1fs of %.1fs submitted; the rest went nowhere",
			got, span.Seconds()+over)
	}
	// And the text of the session that was closed on the way is still there.
	if !strings.Contains(said[0].Text, "atr_1") || !strings.Contains(said[0].Text, "atr_2") {
		t.Fatalf("text %q lost a session", said[0].Text)
	}
}

// A growing transcript is a window in ONE process's memory, and the Service
// address in front of it round-robins per connection. speech runs two replicas,
// so a session opened through the Service and pushed through it again lands
// about half the time on a pod that has never heard of it and answers 404. The
// open says where the session lives; the rest of the turn goes THERE.
//
// Two addresses onto one service, which is what two replicas behind a Service
// is: the open goes in the front door and names the pod, and every push after
// it has to be addressed to that pod.
func TestASessionIsAddressedWhereItLives(t *testing.T) {
	s, door := serve(t, 0)
	pod := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(pod.Close)
	s.lives(pod.URL)

	notes := Record(door, "whisper", "en")
	turn, err := notes.Turn("ana", time.Now())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	turn.Hear(make([]int16, chunk))
	if err := turn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := strings.TrimPrefix(pod.URL, "http://")
	sent := s.addressed()
	if len(sent) == 0 {
		t.Fatal("the turn pushed nothing at all")
	}
	for _, host := range sent {
		if host != want {
			t.Fatalf("a push went to %s; the open said the session lives at %s, "+
				"and behind two replicas that address is a 404 for half the turn",
				host, want)
		}
	}
	t.Logf("%d pushes, all addressed to %s", len(sent), want)
}

// A turn that runs long settles after the short one that followed it. Notes read
// in the order things were SAID, so they are ordered by when a turn began.
func TestNotesReadInTheOrderThingsWereSaid(t *testing.T) {
	_, url := serve(t, 0)
	notes := Record(url, "whisper", "en")

	began := time.Now()
	first, err := notes.Turn("ana", began)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := notes.Turn("ben", began.Add(time.Second))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Hear(make([]int16, rate))
	second.Hear(make([]int16, rate))

	if err := second.Close(); err != nil { // ben settles first
		t.Fatalf("close ben: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close ana: %v", err)
	}

	said := notes.Said()
	if len(said) != 2 || said[0].Speaker != "ana" || said[1].Speaker != "ben" {
		t.Fatalf("notes read %v; ana spoke first", said)
	}
}

// A service too slow to keep up must slow the record, never the room. Frames
// that cannot be queued are dropped and COUNTED: audio discarded quietly is a
// transcript that reads complete and is not.
func TestAServiceThatCannotKeepUpLosesAudioOutLoud(t *testing.T) {
	s, url := serve(t, 0)
	s.hold = make(chan struct{})
	notes := Record(url, "whisper", "en")

	turn, err := notes.Turn("ana", time.Now())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < queue*3; i++ {
		turn.Hear(make([]int16, chunk)) // one chunk of frames each, well past the queue
	}
	lost := notes.Lost()

	// Let the service go before asserting anything. A push still waiting is a
	// request the test server will not shut down without, so an assertion that
	// failed here would hang the run instead of reporting itself.
	close(s.hold)
	if err := turn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if lost == 0 {
		t.Fatal("a stalled service dropped no frames and reported none")
	}
	t.Logf("%d frames dropped while the service was held", lost)
}

// Hear must not block on the network. Every speaker's track is behind that call,
// so a turn that waits for a push stalls the room, and audio arriving on a
// real-time clock while nothing reads it is audio gone.
func TestHearDoesNotWaitForTheService(t *testing.T) {
	s, url := serve(t, 0)
	s.hold = make(chan struct{})
	defer close(s.hold)
	notes := Record(url, "whisper", "en")

	turn, err := notes.Turn("ana", time.Now())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queue*3; i++ {
			turn.Hear(make([]int16, chunk))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Hear blocked on a service that is not answering")
	}
}

// A turn opens with everything its speaker said before the floor was theirs —
// `lead` seconds arriving at once, which is not a rate any room delivers at. A
// queue sized for a room drops what it cannot hold, and what it cannot hold is
// the front of the turn: measured live, cleo's notes began "owe you the
// capacity numbers".
func TestTheOpeningBurstDoesNotOverrunTheTurn(t *testing.T) {
	_, url := serve(t, 0)
	notes := Record(url, "whisper", "en")
	c := started()
	f := chair(c, notes)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	// ben talks under ana for the whole of the delay plus the wait, so his turn
	// opens holding every one of those seconds.
	both := []voice{{"ana", 1}, {"ben", 2}}
	talk(f, c, notice, both...)
	f.Speaking([]string{"ben", "ana"})
	said := talk(f, c, grab, both...) / 2

	// Measured across the handover alone. This replay runs the meeting far
	// faster than a room does, so ordinary frames outrun the pusher here in a
	// way they cannot in a room; the burst is what a room DOES do, and it is
	// what this is about.
	before := notes.Lost()
	f.Speaking([]string{"ben", "ana"})
	if got := notes.Lost() - before; got != 0 {
		t.Fatalf("%d frames dropped opening a turn on %.1fs of held audio; "+
			"the first thing lost is the first thing said", got, float64(said)/rate)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, s := range notes.Said() {
		if s.Speaker == "ben" && s.Seconds < float64(said)/rate {
			t.Fatalf("ben's turn received %.2fs of the %.2fs he had already spoken",
				s.Seconds, float64(said)/rate)
		}
	}
}
