#!/bin/sh
# Every test here claims to catch a specific mistake. This breaks the code that
# way, on purpose, and requires the test to go red — a test that passes against
# the bug it was written for is not evidence of anything.
#
# A build failure does not count as a red test: the mutation has to compile and
# run, or all that has been proven is that Go rejects a typo. Checked below.
set -eu
export PATH=/usr/local/go/bin:$PATH
cd "$(dirname "$0")"

try() { # name, test, file, from, to
	name=$1 test=$2 file=$3 from=$4 to=$5
	cp "$file" "$file.was"
	python3 - "$file" "$from" "$to" <<-'EOF'
		import sys
		p, a, b = sys.argv[1:4]
		s = open(p).read()
		if s.count(a) != 1:
		    sys.exit(f"mutation site appears {s.count(a)} times, wanted 1: {a!r}")
		open(p, "w").write(s.replace(a, b))
	EOF

	if ! go vet ./... >/dev/null 2>&1; then
		mv "$file.was" "$file"
		printf 'BROKEN  %-34s mutation does not compile — proves nothing\n' "$name"
		return 1
	fi
	if go test -run "$test" ./... >/dev/null 2>&1; then
		mv "$file.was" "$file"
		printf 'BLIND   %-34s %s still passes with the bug in\n' "$name" "$test"
		return 1
	fi
	mv "$file.was" "$file"
	printf 'caught  %-34s %s\n' "$name" "$test"
}

try 'turn opens mid-word' TestTurnBeginsBeforeWeKnewWhoseItWas floor.go \
	'lead = 2500 * time.Millisecond' 'lead = 200 * time.Millisecond'

try 'no hysteresis' TestBackchannelDoesNotTakeTheFloor floor.go \
	'return top, now.Sub(f.from) >= grab' 'return top, true'

try 'trusts a stale ranking' TestAMemoryOfASpeakerDoesNotHoldTheFloor floor.go \
	'clips[len(clips)-1].at.After(now.Add(-notice))' 'clips[len(clips)-1].at.After(now.Add(-time.Hour))'

try 'everyone transcribed' TestOnlyTheHolderIsTranscribed floor.go \
	'if speaker == f.holder {' 'if true {'

try 'held audio aliased' TestHeldAudioIsACopy floor.go \
	'pcm: append([]int16(nil), pcm...)' 'pcm: pcm'

try 'floor never freed' TestSilenceEndsTheTurn floor.go \
	'now.Sub(f.spoke) > rest' 'now.Sub(f.spoke) > rest*1000'

try 'refusal swallowed' TestARefusedTurnIsRetriedNotSwallowed floor.go \
	'	case err != nil:
		f.tally.Faults++' '	case err != nil:
		f.holder = who'

try 'talk-over uncounted' TestTalkOverIsCounted floor.go \
	'if f.loud[speaker] {' 'if false {'

try 'attendant on the floor' TestTheAttendantIsNotOnTheFloor attend.go \
	'if _, mine := p.(*lksdk.LocalParticipant); mine {' 'if false {'

try 'turn left open at the end' TestCancellingSettlesTheOpenTurn floor.go \
	'		case <-ctx.Done():
			f.Close()' '		case <-ctx.Done():
			_ = f'



try 'chunks the wrong size' TestAPushIsWholeFramesInsideTheCeiling scribe.go \
	'for len(t.pending) >= chunk {' 'for len(t.pending) >= chunk*3 {'

try 'no rollover at the limit' TestALongTurnRollsOverRatherThanBeingRefused scribe.go \
	'if t.sent+float64(len(pcm))/(rate*2) > span.Seconds() {' 'if false {'

try 'notes read out of order' TestNotesReadInTheOrderThingsWereSaid scribe.go \
	'seq := n.next
	n.next++' 'seq := -n.next
	n.next++'

try 'dropped audio hidden' TestAServiceThatCannotKeepUpLosesAudioOutLoud scribe.go \
	'	default:
		t.to.mu.Lock()
		t.to.lost++
		t.to.mu.Unlock()' '	default:
		t.to.mu.Lock()
		t.to.mu.Unlock()'

# A select with a default never waits, so adding a timeout case to it changes
# nothing — the first version of this mutation was a no-op and the run said so.
# Blocking Hear means taking the default away.
try 'Hear waits for the service' TestHearDoesNotWaitForTheService scribe.go \
	'	select {
	case t.in <- append([]int16(nil), pcm...):
	default:
		t.to.mu.Lock()
		t.to.lost++
		t.to.mu.Unlock()
	}' '	t.in <- append([]int16(nil), pcm...)'

try 'recovered audio still called lost' TestTalkOverThatWinsTheFloorIsNotALoss floor.go \
	'			if held[i].over {
				f.tally.Over -= len(held[i].pcm) // spoken over the last holder, kept anyway
			}' '			if false {
				f.tally.Over -= len(held[i].pcm)
			}'


# The measured failure, pinned to the constant that fixes it: a rival that must
# wait only one observer interval loses to a "mhm, yeah".
try 'grab is one interval' TestBackchannelDoesNotTakeTheFloor floor.go \
	'grab = 2 * time.Second' 'grab = 500 * time.Millisecond'


try 'opening burst frame by frame' TestTheOpeningBurstDoesNotOverrunTheTurn floor.go \
	'			turn.Hear(began)' '			for i := 0; i < len(began); i += 320 {
				stop := i + 320
				if stop > len(began) {
					stop = len(began)
				}
				turn.Hear(began[i:stop])
			}'


try 'audio given to two turns' TestAudioIsGivenToOneTurnOnly floor.go \
	'			if held[i].sent {
				continue
			}' '			if false {
				continue
			}'


try 'wait starts at the report' TestTheWaitRunsFromWhenTheyStartedTalking floor.go \
	'f.rival, f.from = top, f.began(top, now)' 'f.rival, f.from = top, now'


# A session lives in one pod's memory and the Service address in front of it
# round-robins: ignoring where the open says it lives is a 404 for half a turn.
try 'session addressed at the service' TestASessionIsAddressedWhereItLives scribe.go \
	'	if got.At == "" {
		return got.ID, n.url, nil
	}
	return got.ID, got.At, nil' '	return got.ID, n.url, nil'

try 'answers over the speaker' TestTheAttendantWaitsForTheFloor answer.go \
	'	if err := until(ctx, f, free); err != nil {
		return err
	}' '	_ = free'

try 'answer cannot be interrupted' TestSomebodyTalkingCutsTheAnswerShort answer.go \
	'	go func() {
		if until(talking, f, busy) == nil {
			hush()
		}
	}()' '	_ = busy'

try 'answers itself' TestTheAttendantDoesNotAnswerItself answer.go \
	'	if heard == a.answered {
		return nil
	}' '	_ = heard'

try 'forgets what it said' TestWhatTheAttendantSaidIsInTheRecord answer.go \
	'	a.Spoke(a.who, text, time.Now())' '	_ = text'

try 'publishes a container the room cannot carry' TestTheAnswerIsAudioTheRoomCanPlay answer.go \
	'"response_format": "opus"' '"response_format": "mp3"'

echo 'every test earns its keep'
