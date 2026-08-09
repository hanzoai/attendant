# attendant

A server-side participant for a LiveKit room. It joins, subscribes to the audio
every human publishes, and transcribes **whoever holds the floor**.

A meeting has one floor. At any instant one person is talking and the rest are
listening, so a ten-person meeting does not need ten transcripts — it needs one,
handed from speaker to speaker. That is the whole idea, and it is what makes the
cost of transcribing a meeting independent of how many people are in it.

    attendant ──► Floor ──► Scribe ──► hanzoai/speech
     (a room)    (who)      (a turn)   (the words)

- `attend.go` — one room membership. Subscribes, decodes Opus to the 16 kHz mono
  the speech service takes, and reports the room's active speakers.
- `floor.go` — who gets transcribed. No LiveKit in it: the room hands it a
  ranking of identities and frames of audio, and it decides.
- `scribe.go` — turns into transcripts, over hanzoai/speech's growing-transcript
  API. One session at a time, and one settling at a time.

## What the floor has to get right

**A turn begins before you know whose it is.** LiveKit reports a speaker on an
interval and only when the ranking CHANGES, so the first syllables of every turn
are spoken before any word arrives about them. Each speaker's last `lead` of
audio is kept and handed to their turn when they take the floor — in one piece,
because `lead` seconds arriving at once is not a rate any room delivers at, and
a turn that queues audio so the room never waits drops what it cannot hold.
Frame by frame, the first thing dropped is the first thing said.

**A ranking is not a census.** Measured against the live SFU: a speaker stayed
ranked first for thirteen seconds after her last packet, and five seconds passed
with no report at all while somebody talked straight through. Read on its own,
that list is wrong in both directions. So the room says who is LOUDEST, the
tracks say who is still there, and the floor reads both on its own clock.

**A "mhm" is not an interruption.** A rival must be ranked first for `grab`
before the floor moves. One observer interval is not enough: in a live room a
1.5 s "mhm, yeah" took the floor off a speaker mid-sentence, because a
backchannel holds a steady level while a speaker between words does not, and
the room ranked the quieter one first. `grab` is two seconds — longer than
people say "yeah" for. Waiting costs nothing, because `lead` covers
`notice + grab`: whatever the rival said while we waited is still in hand when
they win.

**Nobody pays for our latency twice.** The wait runs from when a rival started
making sound, not from the report that named them. Measured live: cleo began
while ben was finishing, and by the time the room stopped ranking him she had
been talking nearly a second — a wait starting there put the front of "I still
owe you" outside `lead`, and her notes began "owe you the capacity numbers".

**Two people talking is one transcript.** The louder holds the floor; the other
is counted, not transcribed — `Tally.Over` is how much, and it gives audio back
when the interrupter wins the floor and their turn receives it after all, so the
number is the loss and not the overlap. An interjection that matters becomes the
floor and its onset comes out of `lead`, so real turn-taking survives and only
backchannel is dropped. A second stream would put the headcount back in the bill.

## Measured

A meeting of three, then the same meeting with nine more people who came to
listen with their mics open (`TestTheFloorHoldsOneSession`):

| in the room | published | transcribed | turns |
|---|---|---|---|
| 3  | 18.6 s  | 17.0 s | ana, ben, cleo |
| 12 | 198.3 s | 17.0 s | ana, ben, cleo |

Four times the room, the same bill — **11.6× less audio decoded** than a
transcript per person, and the nine who never spoke never opened one.

## Tests

    go test -race ./...          # the floor, on a clock the test drives
    ./mutate.sh                  # breaks each fix, requires its test to go red

Live, with credentials in the environment:

    LK_URL / LK_KEY / LK_SECRET  # the real SFU: TestTheRoomAwardsTheFloor
    SPEECH                       # hanzoai/speech: TestAMeetingBecomesNotes

`fixture/make.sh` builds the multi-party fixture: three voices from
hanzoai/speech saying three different things, so attribution is checked by
content and not by hope.
