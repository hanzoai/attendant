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
	'lead = 1200 * time.Millisecond' 'lead = 200 * time.Millisecond'

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

echo 'every test earns its keep'
