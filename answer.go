// What the attendant says back.
//
// The floor already knows who is talking, so nothing here holds a second
// opinion about turns. A turn ends, the attendant reads the meeting as it
// stands, our model answers it, hanzoai/speech gives that answer a voice, and
// the room hears it — but only while the floor is EMPTY. The floor frees itself
// `rest` after the holder stops, which is the pause a person waits out before
// speaking, and it fills again within `notice` of anyone starting: so an answer
// still playing when somebody talks is cut off there. Interrupting the
// attendant costs a syllable, not a paragraph.
//
// One answer at a time, always formed from the record at the moment it is
// formed. Turns that end while the attendant is thinking do not each queue an
// answer — they are in the record the one answer reads, which is what a person
// catching up would do. And the attendant answers only what it has not answered
// already, so its own voice is never something it replies to.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// How long one answer may take, from asking the model to the last of the
	// audio. Everything in an answer is a call to something else, and a call
	// that never returns is an attendant that never speaks again; this is what
	// makes that a lost answer rather than a wedged process.
	patience = 2 * time.Minute

	// The most a single answer may be. It is going to be READ ALOUD, and the
	// room cannot skim it: this is about a paragraph, past which nobody is
	// listening anyway and the floor should have gone back long ago.
	most = 160
)

// Answer keeps the meeting's record like any scribe, and answers what lands in
// it. It IS the scribe the floor is given, so a turn reaches it only once the
// service has settled the words — there is nothing to reply to before that.
type Answer struct {
	*Notes

	ai     string // our model, at api.hanzo.ai
	model  string
	speech string // hanzoai/speech, for the voice
	voice  string
	prompt string
	who    string // the attendant's own name in the record
	key    string // one identity, from IAM, carried to both

	// A turn landing while an answer is being formed does not ask for a second
	// answer: it is in the record that answer reads. One nudge is as good as
	// three, so this holds one and a full one is dropped.
	wake chan struct{}

	// Turns already answered. Owned by the answering loop — one goroutine, so
	// no lock, and no second answerer to share it with.
	answered int
}

// Turn records the turn as the notes do, and says so when it ends. The nudge is
// on Close and not on Turn because until Close there is no text: a turn is
// answerable only once the service has settled it.
func (a *Answer) Turn(speaker string, at time.Time) (Turn, error) {
	t, err := a.Notes.Turn(speaker, at)
	if err != nil {
		return nil, err
	}
	return &landed{Turn: t, then: a.nudge}, nil
}

// landed is a turn that says so when it ends.
type landed struct {
	Turn
	then func()
}

func (l *landed) Close() error {
	err := l.Turn.Close()
	l.then()
	return err
}

func (a *Answer) nudge() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// tend answers until ctx is cancelled: the whole of the attendant's side of the
// conversation. A failed answer is one answer lost and reported, never the end
// of the meeting — the next turn asks again.
func (a *Answer) tend(ctx context.Context, f *Floor, say func(context.Context, io.ReadCloser) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		}
		if err := a.answer(ctx, f, say); err != nil && ctx.Err() == nil {
			log.Printf("answer: %v", err)
		}
	}
}

// answer forms one answer and says it.
func (a *Answer) answer(ctx context.Context, f *Floor, say func(context.Context, io.ReadCloser) error) error {
	ctx, done := context.WithTimeout(ctx, patience)
	defer done()

	// Nobody speaks over the person holding the floor. Waiting here rather than
	// at the end also means the model reads a meeting that has stopped moving.
	if err := until(ctx, f, free); err != nil {
		return err
	}

	// Only what has not been answered. A nudge left over from a turn that landed
	// while the attendant was talking is answered only if somebody has actually
	// spoken since; otherwise the last word in the record is the attendant's own
	// and it would be replying to itself, and then to that.
	said := a.Said()
	heard := 0
	for _, s := range said {
		if s.Speaker != a.who && s.Text != "" {
			heard++
		}
	}
	if heard == a.answered {
		return nil
	}

	text, err := a.reply(ctx, a.Text())
	if err != nil {
		return err
	}
	if text == "" {
		// The model was asked to answer with nothing when the room is not
		// talking to it. Silence is an answer and costs no floor.
		a.answered = heard
		return nil
	}
	ogg, err := a.speak(ctx, text)
	if err != nil {
		return err
	}

	// Talking. The floor is empty while the attendant holds it — it is dropped
	// from the room's ranking and subscribes to nothing of its own — so the
	// first person to make a sound fills it, and that cancels this.
	talking, hush := context.WithCancel(ctx)
	defer hush()
	go func() {
		if until(talking, f, busy) == nil {
			hush()
		}
	}()
	if err := say(talking, ogg); err != nil {
		return err
	}

	// Filed whether or not it was heard out: the model must not offer again
	// what it was in the middle of saying when somebody cut in.
	a.Spoke(a.who, text, time.Now())
	a.answered = heard
	return nil
}

func free(holder string) bool { return holder == "" }
func busy(holder string) bool { return holder != "" }

// until waits for the floor to be as want says, or for ctx to end.
//
// The floor is read rather than subscribed to: it changes hands on the room's
// clock and not on ours, and `beat` is the interval the floor already looks at
// itself on, so this learns nothing later than the floor knows it.
func until(ctx context.Context, f *Floor, want func(holder string) bool) error {
	t := time.NewTicker(beat)
	defer t.Stop()
	for !want(f.Holder()) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// reply asks our model what to say about the meeting so far.
func (a *Answer) reply(ctx context.Context, meeting string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      a.model,
		"max_tokens": most,
		"messages": []map[string]string{
			{"role": "system", "content": a.prompt},
			{"role": "user", "content": meeting},
		},
	})
	got, err := ask(ctx, a.key, http.MethodPost, a.ai+"/v1/chat/completions", "application/json", body)
	if err != nil {
		return "", err
	}
	var answered struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(got, &answered); err != nil {
		return "", fmt.Errorf("model: %w", err)
	}
	if len(answered.Choices) == 0 {
		return "", nil
	}
	return strings.TrimSpace(answered.Choices[0].Message.Content), nil
}

// speak gives the answer a voice: Ogg/Opus, which is what a room carries and what
// the attendant publishes without re-encoding.
//
// Held in memory rather than streamed from the response, because kokoro
// synthesizes the whole utterance before it answers — there is nothing arriving
// to stream — and a track reading a live response body would stall the room on
// the service instead of on nothing.
func (a *Answer) speak(ctx context.Context, text string) (io.ReadCloser, error) {
	body, _ := json.Marshal(map[string]any{
		"model": "kokoro", "voice": a.voice, "response_format": "opus", "input": text,
	})
	got, err := ask(ctx, a.key, http.MethodPost, a.speech+"/v1/audio/speech", "application/json", body)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(got)), nil
}
