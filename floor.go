// Who gets transcribed.
//
// A meeting has one floor. At any instant one person holds it, and only that
// person's audio is worth sending to a transcriber: the others are listening.
// So the cost of transcribing a meeting is the cost of ONE stream, whether the
// room holds two people or two hundred — the floor is what makes that true.
//
// The room tells us who is speaking; it does not tell us in time. LiveKit's
// observer publishes on an interval, so word of a speaker arrives after their
// first syllables do. A floor that simply switched on that word would clip the
// start of every turn. So each speaker's most recent audio is kept — `lead`
// seconds of it, by wall clock — and handed to their turn when they take the
// floor. The turn therefore begins before we knew whose turn it was.
//
// The same buffer pays for hysteresis. A rival must be the loudest for `grab`
// before the floor moves, which stops a cough or a "mhm" from cutting a
// sentence in half; and because `lead` covers `notice + grab`, the delay costs
// nothing — whatever the rival said while we waited is still in hand when they
// win. That relation is the reason the constants have the values they do, and
// TestLeadCoversTheDelay holds them to it.
package main

import (
	"context"
	"sync"
	"time"
)

const (
	// How long LiveKit can take to report a speaker. livekit-server's audio
	// observer publishes every 500 ms (`audio.update_interval`), so a speaker's
	// first half-second precedes any word we get about them.
	notice = 500 * time.Millisecond

	// How long a rival must be the loudest before the floor moves.
	//
	// This is the whole of what separates an interruption from an agreement, so
	// it has to outlast an agreement. It does not work at one observer interval:
	// a 1.5 s "mhm, yeah" took the floor off a speaker mid-sentence in a live
	// room, because a backchannel holds a steady level while a speaker between
	// words does not, and the room ranked the quieter one first. Two seconds is
	// longer than people say "yeah" for and shorter than they interrupt for.
	grab = 2 * time.Second

	// How much of each speaker's audio is kept back, so that a turn can start
	// before we knew it had. Must cover notice + grab or turns lose their onset.
	//
	// Waiting `grab` costs nothing because of this, and this costs nothing
	// either: what it holds is the rival's own speech from while we waited,
	// which was going nowhere until they won.
	lead = 2500 * time.Millisecond

	// How long the holder can go unreported before the floor is free. Shorter
	// than this and a breath ends the turn; longer and a transcript sits open
	// with nothing arriving.
	rest = 2 * time.Second

	// How often the floor checks whether the holder has fallen silent.
	beat = 250 * time.Millisecond
)

// A Turn is one contiguous span of one speaker's audio.
//
// It maps onto whatever the transcriber calls a session; the floor opens one,
// feeds it, and closes it when the floor moves on. Turns never overlap, so an
// implementation never multiplexes.
type Turn interface {
	// Hear takes audio, in arrival order: a frame as the room delivers it, or
	// the whole of what a speaker said before the floor was theirs, which
	// arrives in one piece when the turn opens.
	//
	// It must not block on the network. Audio arrives on a real-time clock and
	// every speaker's track is behind this call, so a stall here is lost audio,
	// not slower audio. pcm is valid only for the duration of the call.
	Hear(pcm []int16)

	// Close ends the turn and settles its text. It may block; the floor closes
	// turns off the audio path.
	Close() error
}

// A Scribe records a meeting one turn at a time.
type Scribe interface {
	// Turn begins a record of speaker's audio, beginning at at.
	Turn(speaker string, at time.Time) (Turn, error)
}

// Tally is what the floor did, counted rather than claimed. Samples, at the
// room's rate, so seconds are Heard/rate.
type Tally struct {
	Heard  int // samples the room delivered, from every speaker
	Kept   int // samples given to the scribe — the transcriber's whole bill
	Lead   int // of Kept, samples that arrived before their speaker took the floor
	Over   int // samples from someone who was speaking over the holder
	Turns  int // turns opened
	Faults int // scribe calls that failed
}

// clip is one frame, when it arrived, whether it was counted against the holder
// at the time, and whether a turn has had it.
//
// A frame spoken over the holder is a loss until its speaker takes the floor and
// it is handed to their turn after all, so the count has to be able to give it
// back. And a speaker who takes the floor, loses it and takes it again inside
// `lead` still has those frames in hand: sent twice, the meeting notes say what
// they said twice.
type clip struct {
	at   time.Time
	pcm  []int16
	over bool
	sent bool
}

// Floor awards the meeting's floor and gives the holder's audio to a scribe.
// It implements Speech, so an Attendant hands it every speaker's audio; it is
// the floor that decides which of that audio is transcribed.
type Floor struct {
	to  Scribe
	now func() time.Time

	// The room reports each speaker change on its own goroutine, so reports can
	// arrive at once. chair serializes them: opening a turn talks to the scribe,
	// and two reports racing would open two. It is never held on the audio path.
	chair sync.Mutex

	// Closing a turn is a decode of whatever it had not committed, so it happens
	// away from the audio path — and one at a time, which is what bounds a
	// meeting to a single live transcript and a single settling one however
	// often the floor changes hands. drain is how the last of them is waited on.
	settling sync.Mutex
	drain    sync.WaitGroup

	mu     sync.Mutex
	holder string            // who holds the floor; empty means nobody
	turn   Turn              // the holder's open turn
	spoke  time.Time         // when the holder was last reported speaking
	rival  string            // who has been louder than the holder
	from   time.Time         // since when
	keep   map[string][]clip // each speaker's last `lead` of audio
	rank   []string          // the room's last ranking, loudest first
	loud   map[string]bool   // of that ranking, who is still sending audio
	tally  Tally
	fault  error
	done   bool
}

// Chair opens a floor and keeps it until ctx is cancelled. Cancelling closes
// the open turn, so the last thing said in a meeting is transcribed like the
// rest of it.
func Chair(ctx context.Context, to Scribe) *Floor {
	f := floor(to)
	go f.tend(ctx)
	return f
}

// floor is the value; Chair is the value plus the goroutine that watches it.
// Kept apart so that what the floor decides can be exercised on its own clock.
func floor(to Scribe) *Floor {
	return &Floor{to: to, now: time.Now, keep: map[string][]clip{}, loud: map[string]bool{}}
}

// tend gives the floor its own clock — a takeover whose wait has run out, and a
// holder who has stopped — and closes the last turn when the meeting ends.
func (f *Floor) tend(ctx context.Context) {
	t := time.NewTicker(beat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.Close()
			return
		case <-t.C:
			f.look()
		}
	}
}

// Hear takes one speaker's frame. Called from every track's goroutine at once.
func (f *Floor) Hear(speaker string, pcm []int16) {
	now := f.now()

	// The decoder reuses its output buffer, so audio that outlives the call is
	// audio that will be overwritten. Copy once and share the copy.
	held := clip{at: now, pcm: append([]int16(nil), pcm...)}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.tally.Heard += len(pcm)
	f.keep[speaker] = append(since(f.keep[speaker], now.Add(-lead)), held)

	if speaker == f.holder {
		if f.turn != nil {
			f.turn.Hear(held.pcm)
			f.tally.Kept += len(pcm)
			f.keep[speaker][len(f.keep[speaker])-1].sent = true
		}
		return
	}
	// Not the holder. If they are speaking anyway, this is talk-over and it is
	// not transcribed — counted here so the loss is a number and not a guess.
	if f.loud[speaker] {
		f.tally.Over += len(pcm)
		f.keep[speaker][len(f.keep[speaker])-1].over = true
	}
}

// Speaking takes the room's active speakers, loudest first. An empty list means
// the room hears nobody.
//
// What arrives is a ranking, and it is only ever sent when it CHANGES. Nothing
// about it is a statement of the present: measured against the live SFU, five
// seconds passed with no report at all while somebody talked straight through,
// and a speaker who had stopped stayed ranked first for thirteen seconds after
// her last packet. Read either way round, that list on its own is wrong.
//
// So it is kept, and the floor reads it on its own clock against the one signal
// that cannot be stale — whether audio is still arriving. The room says who is
// LOUDEST; the tracks say who is still there.
func (f *Floor) Speaking(loud []string) {
	f.mu.Lock()
	f.rank = append([]string(nil), loud...)
	f.mu.Unlock()
	f.look()
}

// look moves the floor if it should, and frees it if the holder has stopped.
// Every report and every tick comes through here; there is one decision and one
// place that makes it.
func (f *Floor) look() {
	now := f.now()

	f.chair.Lock()
	defer f.chair.Unlock()

	f.mu.Lock()
	loud := f.current(f.rank, now)
	f.loud = make(map[string]bool, len(loud))
	for _, id := range loud {
		f.loud[id] = true
	}
	if f.loud[f.holder] {
		f.spoke = now
	}
	who, take := f.contest(loud, now)
	if !take && f.holder != "" && now.Sub(f.spoke) > rest {
		f.settle(f.turn)
		f.turn, f.holder = nil, ""
	}
	f.mu.Unlock()

	if take {
		f.award(who, now)
	}
}

// current keeps the ranked speakers whose audio is still arriving, in the order
// the room ranked them. Called under the lock.
func (f *Floor) current(loud []string, now time.Time) []string {
	here := make([]string, 0, len(loud))
	for _, who := range loud {
		if f.here(who, now) {
			here = append(here, who)
		}
	}
	return here
}

// here reports whether who's audio is still arriving. Called under the lock.
func (f *Floor) here(who string, now time.Time) bool {
	clips := f.keep[who]
	return len(clips) > 0 && clips[len(clips)-1].at.After(now.Add(-notice))
}

// contest decides whether the floor moves, and to whom. Called under the lock.
func (f *Floor) contest(loud []string, now time.Time) (string, bool) {
	if len(loud) == 0 {
		return "", false // nobody is speaking; rest decides when the floor frees
	}
	top := loud[0]
	if top == f.holder {
		f.rival, f.from = "", time.Time{}
		return "", false
	}
	if f.holder == "" {
		return top, true // an open floor is taken at once; there is nothing to protect
	}
	if f.rival != top {
		f.rival, f.from = top, f.began(top, now)
	}
	return top, now.Sub(f.from) >= grab
}

// began is when the rival's audio starts in what is still held — how long they
// have been talking, as nearly as anything here can know it.
//
// The wait runs from THERE and not from the report that named them, or a rival
// pays for our latency twice: once waiting to be noticed and again waiting to be
// believed. Measured live, cleo began talking while ben was finishing, and by
// the time the room stopped ranking him she had been going nearly a second — a
// wait starting then put the front of "I still owe you" outside `lead`, and her
// notes began "owe you the capacity numbers".
func (f *Floor) began(who string, now time.Time) time.Time {
	clips := since(f.keep[who], now.Add(-lead))
	at := now
	for i := len(clips) - 1; i >= 0; i-- {
		if at.Sub(clips[i].at) > notice {
			break // a gap this long: what came before it was a different stretch
		}
		at = clips[i].at
	}
	return at
}

// award moves the floor to who: opens their turn, hands it the audio they have
// already spoken, and closes the turn that was open.
//
// A scribe that refuses leaves the floor empty rather than held by a speaker
// nobody is recording, so the next report tries again — and their audio is
// still in hand, because that is what `lead` is for.
func (f *Floor) award(who string, now time.Time) {
	turn, err := f.to.Turn(who, now.Add(-lead))

	var stray Turn
	f.mu.Lock()
	f.settle(f.turn)
	f.turn, f.holder, f.spoke = nil, "", now
	f.rival, f.from = "", time.Time{}
	switch {
	case err != nil:
		f.tally.Faults++
	case f.done:
		stray = turn // the meeting ended while this was opening; nobody will feed it
	default:
		f.turn, f.holder = turn, who
		f.tally.Turns++

		// What they had already said, in ONE piece. It is `lead` seconds arriving
		// at once, which is nothing like the pace a room delivers at: handed over
		// frame by frame it is a hundred and twenty-five of them in a moment, and
		// a turn that queues audio so the room never waits drops the overflow —
		// which is the onset, the very thing this exists to protect. Measured
		// live: cleo's turn began "owe you the capacity numbers".
		var began []int16
		held := since(f.keep[who], now.Add(-lead))
		for i := range held {
			if held[i].sent {
				continue
			}
			held[i].sent = true
			began = append(began, held[i].pcm...)
			if held[i].over {
				f.tally.Over -= len(held[i].pcm) // spoken over the last holder, kept anyway
			}
		}
		if len(began) > 0 {
			turn.Hear(began)
			f.tally.Kept += len(began)
			f.tally.Lead += len(began)
		}
	}
	f.mu.Unlock()

	if stray != nil {
		f.shut(stray)
	}
}

// Close ends the open turn and waits for every turn still settling, so that the
// last thing said is in the record before the process is gone. Safe to call
// twice, and safe alongside the cancellation that also calls it.
func (f *Floor) Close() error {
	f.mu.Lock()
	if !f.done {
		f.done = true
		f.settle(f.turn)
		f.turn, f.holder = nil, ""
	}
	f.mu.Unlock()

	f.drain.Wait()

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fault
}

// settle hands a turn off to be closed away from the audio path, one at a time.
//
// Called with f.mu held: a Close that has already passed the lock has already
// taken the turn, so nothing can be handed off that Close will not wait for.
func (f *Floor) settle(t Turn) {
	if t == nil {
		return
	}
	f.drain.Add(1)
	go func() {
		defer f.drain.Done()
		f.settling.Lock()
		defer f.settling.Unlock()
		f.shut(t)
	}()
}

// shut closes a turn, counting and keeping the first failure.
func (f *Floor) shut(t Turn) {
	err := t.Close()
	if err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tally.Faults++
	if f.fault == nil {
		f.fault = err
	}
}

// Tally reports what the floor has done so far.
func (f *Floor) Tally() Tally {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tally
}

// Holder is who has the floor, or empty.
func (f *Floor) Holder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holder
}

// since drops clips older than cut. Reslicing from the front lets append reuse
// the array until it is spent, so a speaker's buffer neither grows nor is
// copied on every frame.
func since(clips []clip, cut time.Time) []clip {
	i := 0
	for i < len(clips) && clips[i].at.Before(cut) {
		i++
	}
	return clips[i:]
}
