package main

import (
	"context"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"sync"
	"testing"
	"time"
)

// clock is a time the test moves by hand, so a rule stated in milliseconds is
// checked in nanoseconds of wall time.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func started() *clock { return &clock{at: time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) pass(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// pad is one frame of audio worth ms milliseconds, filled with a value that
// says which speaker it came from, so a misrouted frame is visible.
func pad(ms int, mark int16) []int16 {
	pcm := make([]int16, rate*ms/1000)
	for i := range pcm {
		pcm[i] = mark
	}
	return pcm
}

// scrap is a Scribe that keeps every turn in memory.
type scrap struct {
	mu    sync.Mutex
	turns []*scrapTurn
	fail  int // refuse this many opens, then succeed
}

// Turns are closed off the audio path, so everything a turn holds is read by
// the test while another goroutine may be closing it. One lock, the scrap's.
type scrapTurn struct {
	to      *scrap
	speaker string
	at      time.Time
	pcm     []int16
	shut    bool
}

func (s *scrap) Turn(speaker string, at time.Time) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail > 0 {
		s.fail--
		return nil, context.DeadlineExceeded
	}
	t := &scrapTurn{to: s, speaker: speaker, at: at}
	s.turns = append(s.turns, t)
	return t, nil
}

func (s *scrap) got() []*scrapTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*scrapTurn(nil), s.turns...)
}

func (t *scrapTurn) Hear(pcm []int16) {
	t.to.mu.Lock()
	defer t.to.mu.Unlock()
	t.pcm = append(t.pcm, pcm...)
}

func (t *scrapTurn) Close() error {
	t.to.mu.Lock()
	defer t.to.mu.Unlock()
	t.shut = true
	return nil
}

func (t *scrapTurn) closed() bool {
	t.to.mu.Lock()
	defer t.to.mu.Unlock()
	return t.shut
}

// marks reports how many samples of each speaker's mark landed in the turn.
func (t *scrapTurn) marks() map[int16]int {
	t.to.mu.Lock()
	defer t.to.mu.Unlock()
	out := map[int16]int{}
	for _, s := range t.pcm {
		out[s]++
	}
	return out
}

// voice is a speaker and the value their frames are filled with, so a frame
// that lands in the wrong turn says whose it was.
type voice struct {
	who  string
	mark int16
}

// talk feeds every voice a frame at a time for d, moving the clock with them.
// Audio always arrives before the room says whose it is, so tests deliver it in
// that order — a report about somebody nothing has been heard from is a report
// about somebody who has stopped talking.
func talk(f *Floor, c *clock, d time.Duration, in ...voice) int {
	n := 0
	for spent := time.Duration(0); spent < d; spent += 20 * time.Millisecond {
		for _, v := range in {
			f.Hear(v.who, pad(20, v.mark))
			n += rate * 20 / 1000
		}
		c.pass(20 * time.Millisecond)
	}
	return n
}

// settles waits for a turn to be closed. Turns close off the audio path, so
// closing is a goroutine away from the call that freed the floor.
func settles(t *scrapTurn) bool {
	for i := 0; i < 200; i++ {
		if t.closed() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// chair is a floor on a clock the test drives. The ticker is left off: rest is
// reconsidered by calling look, so no test waits on wall time.
func chair(c *clock, to Scribe) *Floor {
	f := floor(to)
	f.now = c.now
	return f
}

// The constants are not independent. LiveKit reports a speaker up to `notice`
// late, and the floor then makes a rival wait `grab`; every syllable spoken in
// between has to still be in hand when the turn opens, or the turn starts in the
// middle of a word. That is what `lead` is for, and it is only true if it covers
// both. Shorten lead and this fails — which is the point, because nothing else
// would notice until a transcript quietly lost its first word.
func TestLeadCoversTheDelay(t *testing.T) {
	if lead < notice+grab {
		t.Fatalf("lead %v does not cover notice %v + grab %v: a turn would open mid-word",
			lead, notice, grab)
	}
	t.Logf("lead %v covers notice %v + grab %v with %v to spare", lead, notice, grab, lead-notice-grab)
}

// A room of many, a bill of one: everyone's audio arrives, only the holder's is
// transcribed.
func TestOnlyTheHolderIsTranscribed(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	all := []voice{{"ana", 1}, {"ben", 2}, {"cleo", 3}}
	talk(f, c, 20*time.Millisecond, all...)
	f.Speaking([]string{"ana"})
	talk(f, c, 180*time.Millisecond, all...)

	turns := s.got()
	if len(turns) != 1 {
		t.Fatalf("opened %d turns, wanted 1", len(turns))
	}
	if turns[0].speaker != "ana" {
		t.Fatalf("turn belongs to %q, wanted ana", turns[0].speaker)
	}
	if m := turns[0].marks(); m[2]+m[3] != 0 {
		t.Fatalf("ben/cleo audio reached ana's turn: %v", m)
	}
	got := f.Tally()
	if got.Heard != 3*got.Kept {
		t.Fatalf("heard %d samples and kept %d; three speakers should cost one",
			got.Heard, got.Kept)
	}
	t.Logf("heard %d samples, transcribed %d — %.0f%% saved with 3 in the room",
		got.Heard, got.Kept, 100*(1-float64(got.Kept)/float64(got.Heard)))
}

// "Mhm" while someone is mid-sentence is not a change of floor. A rival that is
// loudest for less than grab must leave the turn alone.
func TestBackchannelDoesNotTakeTheFloor(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	// ana keeps talking. ben mutters over her, loud enough that the room ranks
	// him first — "Mhm. Yeah." runs about a second and a half — long enough that the room
	// ranks him first several times over, which is exactly the case that took
	// the floor off a speaker in a live room when grab was one interval.
	both := []voice{{"ana", 1}, {"ben", 2}}
	const mhm = 1500 * time.Millisecond
	for spent := time.Duration(0); spent < mhm; spent += notice {
		f.Speaking([]string{"ben", "ana"}) // the room ranks the steady mutter first
		talk(f, c, notice, both...)
		if got := f.Holder(); got != "ana" {
			t.Fatalf("floor went to %q after %v of backchannel; a rival owes %v",
				got, spent+notice, grab)
		}
	}
	f.Speaking([]string{"ana"}) // ben stops; ana never did

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	if got := f.Holder(); got != "ana" {
		t.Fatalf("floor went to %q; a backchannel must not take it", got)
	}
	if n := len(s.got()); n != 1 {
		t.Fatalf("opened %d turns; ana's sentence was cut in two", n)
	}
}

// An interruption that persists is a real change of floor, and the interrupter's
// turn must contain what they said BEFORE the room told us they had the floor —
// otherwise every turn starts a word and a half in.
func TestTurnBeginsBeforeWeKnewWhoseItWas(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	// ben starts talking over her. The room says nothing for `notice`, then
	// ranks him first, and the floor still makes him wait `grab`. Everything he
	// says in that whole span has to survive.
	both := []voice{{"ana", 1}, {"ben", 2}}
	said := talk(f, c, notice, both...) / 2
	f.Speaking([]string{"ben", "ana"})
	said += talk(f, c, grab, both...) / 2
	f.Speaking([]string{"ben", "ana"})

	turns := s.got()
	if len(turns) != 2 || turns[1].speaker != "ben" {
		t.Fatalf("floor did not reach ben: %d turns", len(turns))
	}
	if got := turns[1].marks()[2]; got != said {
		t.Fatalf("ben's turn holds %d of the %d samples he had already spoken; "+
			"the missing %.0fms is the front of his first word",
			got, said, float64(said-got)/rate*1000)
	}
	t.Logf("recovered all %.0fms ben spoke before the floor was his",
		float64(said)/rate*1000)
}

// Talk-over that its speaker then wins the floor with is not lost — it is handed
// to their turn out of `lead`. A count that still calls it a loss overstates the
// only number anyone has for how much this design throws away.
func TestTalkOverThatWinsTheFloorIsNotALoss(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	both := []voice{{"ana", 1}, {"ben", 2}}
	talk(f, c, notice, both...) // ben starts; the room has not ranked him yet
	f.Speaking([]string{"ben", "ana"})
	said := talk(f, c, grab, both...) / 2
	f.Speaking([]string{"ben", "ana"}) // and now he takes it

	if got := f.Tally().Over; got != 0 {
		t.Fatalf("%.0fms called talk-over after all %.0fms of it was transcribed",
			float64(got)/rate*1000, float64(said)/rate*1000)
	}
	turns := s.got()
	if len(turns) != 2 || turns[1].marks()[2] < said {
		t.Fatalf("ben's turn holds %d of the %d he spoke over ana", turns[1].marks()[2], said)
	}
}

// A real room keeps naming a speaker after they have stopped. Measured against
// the live SFU: ana was still ranked first thirteen seconds after her last
// packet, while ben was talking. A floor that trusts the ranking alone gives
// her ben's turn and the record is wrong about who said everything after it.
func TestAMemoryOfASpeakerDoesNotHoldTheFloor(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	f.Hear("ana", pad(200, 1))
	f.Speaking([]string{"ana"})
	if f.Holder() != "ana" {
		t.Fatal("ana was talking and did not get the floor")
	}

	// ana stops. The room goes on ranking her first, because that is what it
	// does; ben starts, and every packet from now on is ben's.
	c.pass(time.Second)
	for i := 0; i < 100; i++ {
		f.Hear("ben", pad(20, 2))
		c.pass(20 * time.Millisecond)
		if i%25 == 24 {
			f.Speaking([]string{"ana", "ben"}) // ana still ranked first, still silent
		}
	}

	if got := f.Holder(); got != "ben" {
		t.Fatalf("floor held by %q; ana has not sent a packet in %v", got, time.Second)
	}
	turns := s.got()
	if len(turns) != 2 || turns[1].speaker != "ben" {
		t.Fatalf("turns %v; ben's words belong to ben", turns)
	}
	if m := turns[0].marks(); m[2] != 0 {
		t.Fatalf("%d samples of ben's audio were filed under ana", m[2])
	}
}

// The room hands the same buffer back every packet — the decoder reuses it. Held
// audio must be a copy, or a turn opens holding whatever was decoded last.
func TestHeldAudioIsACopy(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	buf := pad(20, 7)
	f.Hear("ana", buf)
	for i := range buf { // the decoder's next packet, in the same memory
		buf[i] = -7
	}
	c.pass(100 * time.Millisecond)
	f.Speaking([]string{"ana"})

	turns := s.got()
	if len(turns) != 1 {
		t.Fatalf("opened %d turns, wanted 1", len(turns))
	}
	if m := turns[0].marks(); m[7] != len(buf) || m[-7] != 0 {
		t.Fatalf("held audio aliased the decoder's buffer: %v", m)
	}
}

// A holder who stops being reported gives the floor up, and their transcript is
// closed rather than left open for a service that collects idle sessions.
func TestSilenceEndsTheTurn(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	c.pass(rest / 2)
	f.look()
	if f.Holder() != "ana" {
		t.Fatal("floor freed before rest elapsed; a breath would end every turn")
	}

	c.pass(rest)
	f.look()
	if got := f.Holder(); got != "" {
		t.Fatalf("floor still held by %q after %v of silence", got, rest)
	}
	if !settles(s.got()[0]) {
		t.Fatal("turn left open; its text is never settled")
	}
}

// A scribe that refuses must not leave the floor held by a speaker nobody is
// recording. The failure is counted, the floor stays free, and the next report
// tries again — with the audio still in hand.
func TestARefusedTurnIsRetriedNotSwallowed(t *testing.T) {
	c, s := started(), &scrap{fail: 1}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})
	if f.Holder() != "" {
		t.Fatal("floor held after the scribe refused the turn")
	}
	if f.Tally().Faults != 1 {
		t.Fatalf("scribe failure not counted: %+v", f.Tally())
	}

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	turns := s.got()
	if len(turns) != 1 || turns[0].speaker != "ana" {
		t.Fatalf("retry did not open ana's turn: %d turns", len(turns))
	}
	if got, want := turns[0].marks()[1], len(pad(400, 1)); got != want {
		t.Fatalf("retry recovered %d samples of the %d spoken while the scribe was down", got, want)
	}
}

// Two people talking at once: one is transcribed, and what the other said is a
// number rather than a silence nobody mentions.
func TestTalkOverIsCounted(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	both := []voice{{"ana", 1}, {"ben", 2}}
	talk(f, c, 20*time.Millisecond, both...)
	f.Speaking([]string{"ana", "ben"})
	talk(f, c, 200*time.Millisecond, both...)
	got := f.Tally()
	if got.Over != len(pad(200, 2)) {
		t.Fatalf("talk-over counted as %d samples, wanted %d", got.Over, len(pad(200, 2)))
	}
	// Someone merely present and silent is not talk-over.
	f.Hear("cleo", pad(200, 3))
	if f.Tally().Over != got.Over {
		t.Fatal("a listener's track counted as talk-over")
	}
	t.Logf("talk-over %.0fms of %.0fms transcribed",
		float64(got.Over)/rate*1000, float64(got.Kept)/rate*1000)
}

// Cancelling the meeting settles the turn that was open, so the last thing said
// is transcribed like the rest.
func TestCancellingSettlesTheOpenTurn(t *testing.T) {
	s := &scrap{}
	ctx, stop := context.WithCancel(context.Background())
	f := Chair(ctx, s)

	f.Hear("ana", pad(200, 1))
	f.Speaking([]string{"ana"})
	stop()

	for i := 0; i < 100; i++ {
		if turns := s.got(); len(turns) == 1 && turns[0].closed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the open turn was never closed; its audio is transcribed by nobody")
}

// Closing twice, and closing while cancelled, must both be safe: the meeting can
// end by either road.
func TestCloseIsIdempotent(t *testing.T) {
	s := &scrap{}
	ctx, stop := context.WithCancel(context.Background())
	f := Chair(ctx, s)
	f.Hear("ana", pad(200, 1))
	f.Speaking([]string{"ana"})
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	stop()
	time.Sleep(50 * time.Millisecond)
	if n := len(s.got()); n != 1 {
		t.Fatalf("%d turns after two closes and a cancel", n)
	}
}

// heardSpeaking records what reached a listener that wants the active list.
type heardSpeaking struct {
	Speech
	loud []string
}

func (h *heardSpeaking) Hear(string, []int16)   {}
func (h *heardSpeaking) Speaking(loud []string) { h.loud = loud }

// The attendant is in the room but is not in the conversation. LiveKit reports
// it speaking like anyone else when it talks, and a floor awarded to it could
// never be fed: no track of its own is subscribed, so the turn would sit empty
// until rest and take the floor from whoever actually had it.
func TestTheAttendantIsNotOnTheFloor(t *testing.T) {
	to := &heardSpeaking{}
	a := &Attendant{heard: to}
	a.speaking([]lksdk.Participant{&lksdk.LocalParticipant{}})
	if len(to.loud) != 0 {
		t.Fatalf("the attendant reached the floor as %q", to.loud)
	}
}

// A listener that does not want the active list still gets every track — asking
// for the floor is an addition, not a condition.
func TestAListenerNeedNotWatchTheFloor(t *testing.T) {
	a := &Attendant{heard: &collector{}}
	a.speaking([]lksdk.Participant{&lksdk.LocalParticipant{}}) // must not panic
}

// A speaker who loses the floor and takes it back inside `lead` still has those
// seconds in hand. Handed over a second time, the notes say what they said
// twice — and the audio is billed twice for the privilege.
func TestAudioIsGivenToOneTurnOnly(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	// ana holds and talks.
	said := talk(f, c, 400*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})
	said += talk(f, c, 400*time.Millisecond, voice{"ana", 1})

	// She stops; the floor frees, and she starts again well inside `lead`.
	c.pass(rest + beat)
	f.look()
	if f.Holder() != "" {
		t.Fatal("floor still held after rest")
	}
	said += talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := 0
	for _, turn := range s.got() {
		got += turn.marks()[1]
	}
	if got != said {
		t.Fatalf("%d samples reached a turn out of %d she spoke; %.0fms was transcribed twice",
			got, said, float64(got-said)/rate*1000)
	}
	if over := f.Tally().Over; over < 0 {
		t.Fatalf("talk-over counted %d, below zero: audio was given back more than once", over)
	}
}

// A rival is not charged for our latency twice. They start talking; the room
// takes an interval to rank them, and may take longer if the person they are
// interrupting trails off slowly. The wait runs from when they STARTED, so what
// they said in the meantime is inside `lead` and their turn opens on their first
// word rather than a second into it.
func TestTheWaitRunsFromWhenTheyStartedTalking(t *testing.T) {
	c, s := started(), &scrap{}
	f := chair(c, s)

	talk(f, c, 200*time.Millisecond, voice{"ana", 1})
	f.Speaking([]string{"ana"})

	// ben starts, and the room goes on ranking ana first for a full second
	// after — which is what a speaker trailing off looks like.
	both := []voice{{"ana", 1}, {"ben", 2}}
	said := talk(f, c, time.Second, both...) / 2
	f.Speaking([]string{"ben", "ana"})
	said += talk(f, c, grab-time.Second, both...) / 2
	f.Speaking([]string{"ben", "ana"})

	turns := s.got()
	if len(turns) != 2 || turns[1].speaker != "ben" {
		t.Fatalf("turns %v after %v of ben talking; the wait is %v from when he started",
			turns, grab, grab)
	}
	if got := turns[1].marks()[2]; got != said {
		t.Fatalf("ben's turn opens %.0fms in: %d of the %d samples he had spoken",
			float64(said-got)/rate*1000, got, said)
	}
}
