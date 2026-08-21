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

	// What this org can do, read once from /v1/tools. Owned by the same single
	// goroutine as `answered`, so a nil kit means "not read yet" and an empty one
	// means "read, and there is nothing" — two different answers that must not be
	// re-asked on every turn.
	can  kit
	read bool
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

// reply asks our model what to say about the meeting so far, and lets it act on
// what it heard before it speaks.
//
// A meeting that decides something leaves the decision in the transcript, and
// there it stays. The tools the caller's org already offers are how it leaves:
// the model files the item itself, and then says what it did. The loop is bounded
// because a model still calling tools is a model not answering, and the room is
// listening to silence while it does.
func (a *Answer) reply(ctx context.Context, meeting string) (string, error) {
	if !a.read {
		a.can, a.read = offered(ctx, a.ai, a.key), true
	}
	messages := []map[string]any{
		{"role": "system", "content": a.prompt},
		{"role": "user", "content": meeting},
	}
	for round := 0; ; round++ {
		req := map[string]any{
			"model":      a.model,
			"max_tokens": most,
			"messages":   messages,
		}
		// The last round is asked WITHOUT tools, so the turn ends in words rather
		// than in another call nobody hears.
		if len(a.can) > 0 && round < rounds-1 {
			req["tools"] = []map[string]any(a.can)
		}
		body, _ := json.Marshal(req)
		got, err := ask(ctx, a.key, http.MethodPost, a.ai+"/v1/chat/completions", "application/json", body)
		if err != nil {
			return "", err
		}
		var answered struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string          `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(got, &answered); err != nil {
			return "", fmt.Errorf("model: %w", err)
		}
		if len(answered.Choices) == 0 {
			return "", nil
		}
		msg := answered.Choices[0].Message
		// No calls, or no rounds left to make one in: whatever it said is the turn.
		// The room is owed words, and a call it cannot hear is not words.
		if len(msg.ToolCalls) == 0 || round >= rounds-1 {
			return strings.TrimSpace(msg.Content), nil
		}
		// The assistant turn goes back verbatim — a tool result with no call
		// preceding it is a conversation the model cannot read.
		var calls []map[string]any
		for _, c := range msg.ToolCalls {
			calls = append(calls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Function.Name, "arguments": string(c.Function.Arguments)},
			})
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": msg.Content, "tool_calls": calls})
		for _, c := range msg.ToolCalls {
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": c.ID,
				"content":      call(ctx, a.ai, a.key, c.Function.Name, c.Function.Arguments),
			})
		}
	}
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
