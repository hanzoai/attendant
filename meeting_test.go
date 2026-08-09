package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A meeting, replayed frame by frame.
//
// The fixture is real speech: three voices from hanzoai/speech, saying three
// different things, in Ogg Opus — the container a browser publishes — decoded
// through the same path the attendant decodes a track through.
//
// What the room reports is modelled here rather than mocked. livekit-server's
// observer looks at the audio level each publisher stamps on its packets,
// every `notice`, and names who was loud in the interval just past, loudest
// first. That is what `report` computes, and it is why a speaker's first
// half-second is always spoken before anyone hears about it.
//
// The model is not taken on faith: TestTheRoomAwardsTheFloor runs the same
// script through the real SFU and checks the floor lands in the same hands.

// frame is the audio a track carries in 20 ms, which is what Opus packetizes.
const frame = rate / 50

// said is one speaker's turn in the script.
type said struct {
	who  string
	file string
	at   time.Duration // when they start talking
	gain float64       // how loud, against a speaker holding the floor at 1
	loop time.Duration // an open mic: keeps publishing this long, never louder
	pcm  []int16
}

// crowd adds the n people who came to listen. Their mics are open, as mics in a
// meeting are, so their tracks arrive all the way through — far too quiet for
// the room to call any of them a speaker, and far too loud for a transcriber to
// ignore if anybody points one at it. This is the room a per-speaker design
// pays for: nine open mics is nine transcripts of a room with a television on.
func crowd(in []said, n int, until time.Duration) []said {
	out := append([]said(nil), in...)

	// About 34 dB under the person holding the floor: a voice heard across a
	// room, well below any level at which an observer would call it a speaker.
	// Louder than this and the room is right to call them a speaker, and the
	// floor is right to award it to them.
	const gain = 0.02
	pcm := make([]int16, len(in[0].pcm))
	for i, s := range in[0].pcm {
		pcm[i] = int16(math.Round(float64(s) * gain))
	}
	for i := 0; i < n; i++ {
		out = append(out, said{
			who:  fmt.Sprintf("guest%d", i),
			file: in[0].file,
			at:   time.Duration(i) * 700 * time.Millisecond,
			gain: gain,
			loop: until,
			pcm:  pcm,
		})
	}
	return out
}

// script is a meeting: two colleagues reporting, one backchannel that must not
// interrupt, and one interruption that must.
//
// The backchannel is quieter, because that is what a backchannel is — an
// agreement muttered while someone else is talking, not a bid for the floor.
// Loudness is the only thing the room ranks speakers by, so it is the only
// thing that can tell the two apart, and it is applied to the audio itself so
// that the replay and the live room rank it identically.
func script(t *testing.T) []said {
	t.Helper()
	in := []said{
		{who: "ana", file: "fixture/ana.ogg", at: 0, gain: 1},
		{who: "ben", file: "fixture/nod.ogg", at: 3 * time.Second, gain: 0.4},
		{who: "ben", file: "fixture/ben.ogg", at: 9 * time.Second, gain: 1},
		{who: "cleo", file: "fixture/cleo.ogg", at: 15 * time.Second, gain: 1.2},
	}
	for i := range in {
		in[i].pcm = decodeOgg(t, in[i].file)
		if len(in[i].pcm) == 0 {
			t.Skipf("fixture %s is empty; run fixture/make.sh", in[i].file)
		}
		if in[i].gain != 1 {
			for j, s := range in[i].pcm {
				in[i].pcm[j] = int16(math.Round(float64(s) * in[i].gain))
			}
		}
	}
	return in
}

// loud is what the room would report at time at: everyone who sent audio in the
// interval just past, loudest first.
func (m *meeting) loud(at time.Duration) []string {
	type level struct {
		who string
		rms float64
	}
	var seen []level
	for who, pcm := range m.window {
		if len(pcm) == 0 {
			continue
		}
		var sum float64
		for _, s := range pcm {
			sum += float64(s) * float64(s)
		}
		seen = append(seen, level{who, math.Sqrt(sum / float64(len(pcm)))})
		m.window[who] = nil
	}
	// A publisher below the observer's threshold is not reported at all; silence
	// decoded from a gap is not somebody speaking.
	for i := 0; i < len(seen); i++ {
		for j := i + 1; j < len(seen); j++ {
			if seen[j].rms > seen[i].rms {
				seen[i], seen[j] = seen[j], seen[i]
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, s := range seen {
		if s.rms >= 200 { // below this is a gap between words, not a voice
			out = append(out, s.who)
		}
	}
	return out
}

type meeting struct {
	window map[string][]int16
	heard  map[string]int // audio each speaker actually published
}

// play runs the script through f. When live, it runs on the wall clock, which
// is what a room does and what a scribe pushing to a service must keep up with.
func play(t *testing.T, f *Floor, c *clock, in []said, live bool) *meeting {
	t.Helper()
	m := &meeting{window: map[string][]int16{}, heard: map[string]int{}}

	var last time.Duration
	for _, s := range in {
		if end := s.at + s.loop + time.Duration(len(s.pcm))*time.Second/rate; end > last {
			last = end
		}
	}

	began := time.Now()
	for step := time.Duration(0); step < last; step += 20 * time.Millisecond {
		for _, s := range in {
			off := int((step - s.at) * rate / time.Second)
			if off >= 0 && s.loop > 0 && step-s.at < s.loop {
				off %= len(s.pcm) - frame // an open mic does not stop
			}
			if off < 0 || off+frame > len(s.pcm) {
				continue // not talking: a gated mic publishes nothing
			}
			pcm := s.pcm[off : off+frame]
			f.Hear(s.who, pcm)
			m.window[s.who] = append(m.window[s.who], pcm...)
			m.heard[s.who] += frame
		}
		c.pass(20 * time.Millisecond)
		if (step+20*time.Millisecond)%notice == 0 {
			f.Speaking(m.loud(step))
		}
		if live {
			if wait := time.Until(began.Add(step)); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	return m
}

// counting is a Scribe that records nothing but what it was asked to record and
// how much of it was going on at once.
//
// Closing takes a moment here, as it does on a service that has to decode what
// the turn had not committed. That is what makes the count mean anything: if
// closes ran concurrently this would see them.
type counting struct {
	mu       sync.Mutex
	open     int // opened and not yet closed
	shut     int // closing right now
	mostOpen int
	mostShut int
	turns    []string
}

func (c *counting) Turn(speaker string, _ time.Time) (Turn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open++
	if c.open > c.mostOpen {
		c.mostOpen = c.open
	}
	c.turns = append(c.turns, speaker)
	return &countingTurn{to: c}, nil
}

func (c *counting) peak() (open, shut int, turns []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mostOpen, c.mostShut, append([]string(nil), c.turns...)
}

type countingTurn struct {
	to   *counting
	once sync.Once
}

func (t *countingTurn) Hear([]int16) {}

func (t *countingTurn) Close() error {
	t.once.Do(func() {
		t.to.mu.Lock()
		t.to.shut++
		if t.to.shut > t.to.mostShut {
			t.to.mostShut = t.to.shut
		}
		t.to.mu.Unlock()

		time.Sleep(30 * time.Millisecond) // a decode of what was not committed

		t.to.mu.Lock()
		t.to.shut--
		t.to.open--
		t.to.mu.Unlock()
	})
	return nil
}

// What the floor is for, measured on real speech rather than asserted.
//
// Two numbers matter and they are not the same. The one that decouples meeting
// size from estate capacity is CONCURRENT SESSIONS: a transcript per speaker is
// one open session per person in the room, and the estate has four decode
// workers in total, so a single ten-person meeting oversubscribes it. The floor
// holds one, whatever the headcount.
//
// The second is audio submitted. That saving is whatever people actually talk
// over each other by — here, a backchannel and an interruption, both real
// speech — plus what the floor spends on `lead` to not clip anyone's first word.
func TestTheFloorHoldsOneSession(t *testing.T) {
	base := script(t)
	var last time.Duration
	for _, s := range base {
		if end := s.at + time.Duration(len(s.pcm))*time.Second/rate; end > last {
			last = end
		}
	}

	sec := func(n int) float64 { return float64(n) / rate }
	var small, large Tally
	var peaks []int

	for _, room := range []struct {
		name   string
		guests int
	}{{"the three who talk", 0}, {"and nine who came to listen", 9}} {
		in := crowd(base, room.guests, last)
		c, s := started(), &counting{}
		f := chair(c, s)
		m := play(t, f, c, in, false)
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		got := f.Tally()
		open, shut, turns := s.peak()
		people := map[string]bool{}
		for _, s := range in {
			people[s.who] = true
		}

		// The floor's own count of what the room delivered, against the script's.
		// A tally that counts something other than the audio it was given is a
		// saving measured against nothing.
		published := 0
		for _, n := range m.heard {
			published += n
		}
		if got.Heard != published {
			t.Fatalf("floor counted %d samples heard; the script published %d", got.Heard, published)
		}
		if shut > 1 {
			t.Fatalf("%d transcripts settling at once; closes are serialized so this is 1", shut)
		}
		peaks = append(peaks, open)

		t.Logf("%d in the room — %s", len(people), room.name)
		t.Logf("   published   %6.1fs of audio, from %d tracks", sec(got.Heard), len(people))
		t.Logf("   transcribed %6.1fs  (%.0f%% of it), in %d turns: %v",
			sec(got.Kept), 100*sec(got.Kept)/sec(got.Heard), got.Turns, turns)
		t.Logf("   sessions    %d open at a peak, %d settling at a time — a transcript per person is %d",
			open, shut, len(people))
		t.Logf("   lead        %6.1fs recovered from before its speaker held the floor", sec(got.Lead))
		t.Logf("   talk-over   %6.1fs published by someone the room called a speaker, not transcribed",
			sec(got.Over))

		if room.guests == 0 {
			small = got
		} else {
			large = got
		}
		if len(turns) < 3 {
			t.Fatalf("turns %v: ana, ben and cleo each held the floor", turns)
		}
	}

	// The point of the whole thing: four times the room, the same bill — in
	// sessions and in audio alike. A transcript per person would have cost
	// everything on the Heard line.
	if peaks[1] > peaks[0] {
		t.Fatalf("sessions peaked at %d with twelve in the room against %d with three; "+
			"the transcriber still pays for the headcount", peaks[1], peaks[0])
	}
	if large.Kept != small.Kept {
		t.Fatalf("transcribed %.1fs with twelve in the room against %.1fs with three; "+
			"the cost still follows the headcount", sec(large.Kept), sec(small.Kept))
	}
	t.Logf("twelve in the room cost the same %.1fs as three; a transcript per person "+
		"would have cost %.1fs against %.1fs — %.1fx",
		sec(large.Kept), sec(large.Heard), sec(small.Heard),
		float64(large.Heard)/float64(small.Heard))
	t.Logf("against a transcript per person, this meeting decodes %.1fx less audio",
		float64(large.Heard)/float64(large.Kept))
}

// The record, read back from the service that made the audio in the first place.
//
// This is the test that says whether the floor is worth anything: a transcript
// that is right about WHAT was said and wrong about WHO said it is not meeting
// notes. Each speaker's sentence has to come back under their own name, and the
// first word of each has to be in it — that is the one the naive design loses,
// because the room does not name a speaker until they are already talking.
func TestTheRecordNamesWhoSaidIt(t *testing.T) {
	speech := os.Getenv("SPEECH")
	if speech == "" {
		t.Skip("no SPEECH endpoint (kubectl -n hanzo port-forward svc/speech 8799:80)")
	}
	in := script(t)

	notes := Record(speech, "whisper", "en")
	c := started()
	f := chair(c, notes)
	_ = play(t, f, c, in, true) // wall clock: the scribe has to keep up with the room
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	said := notes.Said()
	if notes.Lost() > 0 {
		t.Fatalf("%d frames dropped: the service could not keep up with one stream", notes.Lost())
	}
	for _, s := range said {
		t.Logf("%-5s %5.1fs  %s", s.Speaker, s.Seconds, s.Text)
	}

	// Every speaker's own words, under their own name. The opening word of each
	// is checked on its own: it is the one that goes missing.
	for _, want := range []struct{ who, first, phrase string }{
		{"ana", "the index", "rebuild"},
		{"ben", "then we can", "release"},
		{"cleo", "i still owe", "capacity"},
	} {
		var mine string
		for _, s := range said {
			if s.Speaker == want.who {
				mine += " " + strings.ToLower(s.Text)
			}
		}
		if mine == "" {
			t.Errorf("%s held the floor but the record has nothing under their name", want.who)
			continue
		}
		if !strings.Contains(mine, want.phrase) {
			t.Errorf("%s's turn does not contain %q: %s", want.who, want.phrase, mine)
		}
		if !strings.Contains(mine, want.first) {
			t.Errorf("%s's turn is missing its opening words %q — the front of the turn was clipped: %s",
				want.who, want.first, mine)
		}
	}

	// Nobody's words under anybody else's name.
	for _, s := range said {
		for who, theirs := range map[string]string{"ana": "shard", "ben": "migration", "cleo": "capacity"} {
			if s.Speaker != who && strings.Contains(strings.ToLower(s.Text), theirs) {
				t.Errorf("%q is attributed to %s but is %s's line", s.Text, s.Speaker, who)
			}
		}
	}
	fmt.Print(notes.Text())
}

// The whole thing, with nothing modelled: three people publish real speech into
// the real SFU, the attendant subscribes to all of it, the floor picks whoever
// holds it, and hanzoai/speech reads back what was said. Notes at the end, with
// names on them.
func TestAMeetingBecomesNotes(t *testing.T) {
	key, secret, url := os.Getenv("LK_KEY"), os.Getenv("LK_SECRET"), os.Getenv("LK_URL")
	speech := os.Getenv("SPEECH")
	if key == "" || secret == "" || url == "" || speech == "" {
		t.Skip("needs LiveKit credentials and a SPEECH endpoint")
	}
	in := script(t)
	room := fmt.Sprintf("notes%d", time.Now().UnixNano())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	notes := Record(speech, "whisper", "en")
	f := Chair(ctx, notes)
	a, err := Attend(ctx, url, mint(key, secret, room, "attendant", 10*time.Minute), f)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer a.Leave()

	var wg sync.WaitGroup
	for _, who := range []string{"ana", "ben", "cleo"} {
		var mine []said
		for _, s := range in {
			if s.who == who {
				mine = append(mine, s)
			}
		}
		wg.Add(1)
		go func(who string, lines []said) {
			defer wg.Done()
			if err := publish(ctx, url, mint(key, secret, room, who, 10*time.Minute), lines); err != nil {
				t.Errorf("%s: %v", who, err)
			}
		}(who, mine)
	}
	wg.Wait()
	time.Sleep(rest + time.Second)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := f.Tally()
	t.Logf("%.1fs published by the room, %.1fs transcribed, %d turns, %d frames lost",
		float64(got.Heard)/rate, float64(got.Kept)/rate, got.Turns, notes.Lost())
	fmt.Print(notes.Text())

	if notes.Lost() > 0 {
		t.Errorf("%d frames dropped: the service could not keep up with one stream", notes.Lost())
	}
	for _, want := range []struct{ who, first, phrase string }{
		{"ana", "index rebuild", "shard"},
		{"ben", "then we can", "migration"},
		{"cleo", "i still owe", "capacity"},
	} {
		var mine string
		for _, s := range notes.Said() {
			if s.Speaker == want.who {
				mine += " " + strings.ToLower(s.Text)
			}
		}
		if !strings.Contains(mine, want.phrase) {
			t.Errorf("%s said %q into a real room; the notes have %q", want.who, want.phrase, mine)
		}
		if !strings.Contains(mine, want.first) {
			t.Errorf("%s's turn is missing its opening words %q: %q", want.who, want.first, mine)
		}
	}
	for _, s := range notes.Said() {
		for who, theirs := range map[string]string{"ana": "shard", "ben": "migration", "cleo": "capacity"} {
			if s.Speaker != who && strings.Contains(strings.ToLower(s.Text), theirs) {
				t.Errorf("%q is filed under %s and belongs to %s", s.Text, s.Speaker, who)
			}
		}
	}
}

// witness records what a real room actually says, so the floor is judged
// against the signal rather than against an idea of it.
type witness struct {
	mu    sync.Mutex
	began time.Time
	audio map[string]int
	says  []string
}

func (w *witness) Hear(speaker string, pcm []int16) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.audio[speaker] += len(pcm)
}

func (w *witness) Speaking(loud []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.says = append(w.says, fmt.Sprintf("%5.1fs %v", time.Since(w.began).Seconds(), loud))
}

// What the room reports, before anything is decided about it. A floor built on
// a signal nobody has looked at is a floor built on an assumption.
func TestTheRoomReportsSpeakers(t *testing.T) {
	key, secret, url := os.Getenv("LK_KEY"), os.Getenv("LK_SECRET"), os.Getenv("LK_URL")
	if key == "" || secret == "" || url == "" {
		t.Skip("no LiveKit credentials in env")
	}
	in := script(t)
	room := fmt.Sprintf("watch%d", time.Now().UnixNano())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	w := &witness{began: time.Now(), audio: map[string]int{}}
	a, err := Attend(ctx, url, mint(key, secret, room, "attendant", 5*time.Minute), w)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer a.Leave()

	var wg sync.WaitGroup
	for _, who := range []string{"ana", "ben", "cleo"} {
		var mine []said
		for _, s := range in {
			if s.who == who {
				mine = append(mine, s)
			}
		}
		wg.Add(1)
		go func(who string, lines []said) {
			defer wg.Done()
			for _, l := range lines {
				t.Logf("%5.1fs %s publishes %s at level %d",
					l.at.Seconds(), who, l.file, dBov(l.pcm))
			}
			if err := publish(ctx, url, mint(key, secret, room, who, 5*time.Minute), lines); err != nil {
				t.Errorf("%s: %v", who, err)
			}
		}(who, mine)
	}
	wg.Wait()
	time.Sleep(2 * time.Second)

	w.mu.Lock()
	defer w.mu.Unlock()
	for who, n := range w.audio {
		t.Logf("subscribed %-6s %5.1fs of audio", who, float64(n)/rate)
	}
	for _, s := range w.says {
		t.Log("reported ", s)
	}
	if len(w.says) == 0 {
		t.Fatal("the room reported no speaker at all")
	}
}

// The same script through the real SFU. The model above says the room reports a
// speaker late and sorted by level; this checks that against livekit itself, by
// publishing the fixture from three participants and reading back which of them
// the floor gave the turn to.
func TestTheRoomAwardsTheFloor(t *testing.T) {
	key, secret, url := os.Getenv("LK_KEY"), os.Getenv("LK_SECRET"), os.Getenv("LK_URL")
	if key == "" || secret == "" || url == "" {
		t.Skip("no LiveKit credentials in env")
	}
	in := script(t)
	room := fmt.Sprintf("floor%d", time.Now().UnixNano())

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	s := &counting{}
	f := Chair(ctx, s)
	a, err := Attend(ctx, url, mint(key, secret, room, "attendant", 5*time.Minute), f)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer a.Leave()

	// One publisher per person, each saying their own line at their own time.
	var wg sync.WaitGroup
	for _, who := range []string{"ana", "ben", "cleo"} {
		mine := []said{}
		for _, s := range in {
			if s.who == who {
				mine = append(mine, s)
			}
		}
		wg.Add(1)
		go func(who string, lines []said) {
			defer wg.Done()
			if err := publish(ctx, url, mint(key, secret, room, who, 5*time.Minute), lines); err != nil {
				t.Errorf("%s: %v", who, err)
			}
		}(who, mine)
	}
	wg.Wait()
	time.Sleep(rest + time.Second) // let the last turn settle
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	open, shut, turns := s.peak()
	got := f.Tally()
	t.Logf("live: %.1fs published, %.1fs transcribed, %d sessions open / %d settling, turns %v",
		float64(got.Heard)/rate, float64(got.Kept)/rate, open, shut, turns)
	if shut > 1 {
		t.Fatalf("%d transcripts settling at once against the real room", shut)
	}
	if len(turns) == 0 {
		t.Fatal("the real room never reported a speaker: the floor was never awarded")
	}
	for _, who := range []string{"ana", "ben", "cleo"} {
		found := false
		for _, got := range turns {
			found = found || got == who
		}
		if !found {
			t.Errorf("%s spoke but never held the floor: turns were %v", who, turns)
		}
	}
}
