package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The attendant's own side of a meeting, driven on a clock the test moves: a
// model that answers, a voice that returns the repo's own Ogg/Opus, and a floor
// with real speakers on it. What is measured is WHEN it speaks, because an
// attendant that talks over people is not a participant.

// room is where an answer goes when there is no SFU. It takes what would have
// been published and, like a real room, holds the attendant for as long as the
// audio would have taken — so being cut off is something that can happen here.
type room struct {
	hold time.Duration // how long the audio plays for
	done chan struct{} // one signal per answer said

	mu   sync.Mutex
	said [][]byte
	cut  []bool
}

func (r *room) say(ctx context.Context, ogg io.ReadCloser) error {
	heard, err := io.ReadAll(ogg)
	if err != nil {
		return err
	}
	cut := false
	if r.hold > 0 {
		select {
		case <-time.After(r.hold):
		case <-ctx.Done():
			cut = true
		}
	}
	r.mu.Lock()
	r.said = append(r.said, heard)
	r.cut = append(r.cut, cut)
	r.mu.Unlock()

	select {
	case r.done <- struct{}{}:
	default:
	}
	return nil
}

func (r *room) heard() ([][]byte, []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.said...), append([]bool(nil), r.cut...)
}

// answering is the attendant's voice against the fake: one URL for the model
// and for the voice, because the estate is one and it is reached one way.
func answering(notes *Notes, url string) *Answer {
	return &Answer{
		Notes: notes, ai: url, model: "best", speech: url, voice: "af_heart",
		prompt: "answer", who: "attendant", wake: make(chan struct{}, 1),
	}
}

// spoke drives one whole turn: somebody talks, the room names them, and then
// the room falls quiet — which is what closes their turn and frees the floor.
func spoke(f *Floor, c *clock, who string) {
	talk(f, c, time.Second, voice{who, 1})
	f.Speaking([]string{who})
	talk(f, c, 500*time.Millisecond, voice{who, 1})
	c.pass(rest + time.Second)
	f.Speaking(nil)
}

// waited reports whether an answer arrived inside d.
func waited(r *room, d time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

// The floor moves from ana to ben, which ends ana's turn — and the attendant
// now has something to answer while somebody else is talking. It waits.
//
// This is the whole difference between a participant and a nuisance: a turn
// ending is not an invitation, an empty floor is.
func TestTheAttendantWaitsForTheFloor(t *testing.T) {
	s, url := serve(t, 0)
	s.says("The rebuild finished at four.")
	notes := Record(url, "whisper", "en")
	say := answering(notes, url)

	c := started()
	f := chair(c, say)
	r := &room{done: make(chan struct{}, 4)}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go say.tend(ctx, f, r.say)

	// ana holds the floor, then ben takes it off her: her turn closes and asks
	// for an answer, and ben is mid-sentence.
	talk(f, c, time.Second, voice{"ana", 1})
	f.Speaking([]string{"ana"})
	both := []voice{{"ana", 1}, {"ben", 2}}
	talk(f, c, notice, both...)
	f.Speaking([]string{"ben", "ana"})
	talk(f, c, grab, both...)
	f.Speaking([]string{"ben", "ana"})
	if f.Holder() != "ben" {
		t.Fatalf("ben holds the floor here, not %q", f.Holder())
	}

	if waited(r, time.Second) {
		t.Fatal("the attendant answered ana while ben was talking")
	}

	// ben stops. Now there is a gap, and a gap is what an answer is for.
	c.pass(rest + time.Second)
	f.Speaking(nil)
	if !waited(r, 10*time.Second) {
		t.Fatal("the floor was free and the attendant never answered")
	}
	said, _ := r.heard()
	t.Logf("held its answer through ben's turn, then said %d bytes of audio into the gap", len(said[0]))
}

// Somebody starts talking while the attendant is answering. It stops.
//
// The floor is empty while the attendant talks — it is not on the ranking and
// subscribes to nothing of its own — so the first person to make a sound fills
// it, and that is the signal. Being interrupted costs a syllable.
func TestSomebodyTalkingCutsTheAnswerShort(t *testing.T) {
	s, url := serve(t, 0)
	s.says("I can pull those numbers up.")
	notes := Record(url, "whisper", "en")
	say := answering(notes, url)

	c := started()
	f := chair(c, say)
	// A minute of audio: far longer than this test will wait, so an answer that
	// finishes here finished because it was cut off.
	r := &room{hold: time.Minute, done: make(chan struct{}, 4)}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go say.tend(ctx, f, r.say)

	spoke(f, c, "ana")

	// Wait for the attendant to be talking: the model has answered and the audio
	// is playing when the service has been asked.
	for at := time.Now(); len(s.asks()) == 0; {
		if time.Since(at) > 10*time.Second {
			t.Fatal("the attendant never started answering")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ben talks over it.
	talk(f, c, notice, voice{"ben", 2})
	f.Speaking([]string{"ben"})
	if f.Holder() != "ben" {
		t.Fatalf("ben holds the floor here, not %q", f.Holder())
	}

	if !waited(r, 5*time.Second) {
		t.Fatal("ben started talking and the attendant kept going")
	}
	_, cut := r.heard()
	if !cut[0] {
		t.Fatal("the answer played to the end while ben was talking")
	}
	t.Log("stopped mid-answer when ben took the floor")
}

// A turn that ends while the attendant is talking asks for an answer, and by
// the time that ask is read the attendant has already spoken. Nobody answers
// themselves — and an attendant that did would then answer THAT, forever.
func TestTheAttendantDoesNotAnswerItself(t *testing.T) {
	s, url := serve(t, 0)
	s.says("Green across both regions.")
	notes := Record(url, "whisper", "en")
	say := answering(notes, url)

	c := started()
	f := chair(c, say)
	r := &room{done: make(chan struct{}, 4)}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go say.tend(ctx, f, r.say)

	spoke(f, c, "ana")
	if !waited(r, 10*time.Second) {
		t.Fatal("ana spoke and the attendant did not answer")
	}

	// The leftover ask, with nothing said since.
	say.nudge()
	if waited(r, time.Second) {
		t.Fatal("the attendant answered its own answer")
	}
	if n := len(s.asks()); n != 1 {
		t.Fatalf("the model was asked %d times for one thing said", n)
	}
	t.Log("one turn, one answer, and the leftover ask went nowhere")
}

// What the attendant said is in the record under its own name, and it is in
// what the model reads next. A participant that cannot hear itself repeats
// itself.
func TestWhatTheAttendantSaidIsInTheRecord(t *testing.T) {
	s, url := serve(t, 0)
	const reply = "Both regions are green."
	s.says(reply)
	notes := Record(url, "whisper", "en")
	say := answering(notes, url)

	c := started()
	f := chair(c, say)
	r := &room{done: make(chan struct{}, 4)}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go say.tend(ctx, f, r.say)

	spoke(f, c, "ana")
	if !waited(r, 10*time.Second) {
		t.Fatal("ana spoke and the attendant did not answer")
	}
	spoke(f, c, "ben")
	if !waited(r, 10*time.Second) {
		t.Fatal("ben spoke and the attendant did not answer")
	}

	kept := notes.Text()
	if !strings.Contains(kept, "attendant: "+reply) {
		t.Fatalf("the record does not say what the attendant said:\n%s", kept)
	}
	asked := s.asks()
	if len(asked) < 2 {
		t.Fatalf("the model was asked %d times across two turns", len(asked))
	}
	if !strings.Contains(asked[1], reply) {
		t.Fatalf("the second thing the model read does not contain what it already said:\n%s", asked[1])
	}
	t.Logf("the record the model reads on the second turn:\n%s", asked[1])
}

// What is published is the audio the voice made, and it is audio a room can
// play: real Ogg/Opus that decodes to speech at the rate the estate speaks in.
func TestTheAnswerIsAudioTheRoomCanPlay(t *testing.T) {
	s, url := serve(t, 0)
	s.says("Say something back.")
	notes := Record(url, "whisper", "en")
	say := answering(notes, url)

	c := started()
	f := chair(c, say)
	r := &room{done: make(chan struct{}, 4)}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go say.tend(ctx, f, r.say)

	spoke(f, c, "ana")
	if !waited(r, 10*time.Second) {
		t.Fatal("ana spoke and the attendant did not answer")
	}

	said, _ := r.heard()
	pcm := decode(t, bytes.NewReader(said[0]))
	seconds := float64(len(pcm)) / rate
	if seconds < 3.5 {
		t.Fatalf("published %d bytes that decode to %.2fs; the voice answered with 3.9s of speech",
			len(said[0]), seconds)
	}
	var sum float64
	for _, v := range pcm {
		sum += float64(v) * float64(v)
	}
	if rms := sum / float64(len(pcm)); rms < 200*200 {
		t.Fatalf("what was published decodes to silence, not speech")
	}
	t.Logf("published %d bytes of Ogg/Opus: %.2fs of speech", len(said[0]), seconds)
}
