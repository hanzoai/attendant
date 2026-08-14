# attendant

A participant in a LiveKit room that is not a person. It joins, subscribes to
the audio every human publishes, transcribes **whoever holds the floor**, and
answers them out loud.

A meeting has one floor. At any instant one person is talking and the rest are
listening, so a ten-person meeting does not need ten transcripts — it needs one,
handed from speaker to speaker. That is the whole idea, and it is what makes the
cost of transcribing a meeting independent of how many people are in it. It is
also what tells the attendant when it may speak: the floor is empty.

    hears     attendant ──► Floor ──► Scribe ──► hanzoai/speech
              (a room)      (who)     (a turn)   (the words)

    answers   api.hanzo.ai ──► hanzoai/speech ──► Say
              (what to say)    (the voice)        (into the gap)

- `attend.go` — one room membership. Subscribes, decodes Opus to the 16 kHz mono
  the speech service takes, reports the room's active speakers, and publishes an
  answer back into the room.
- `floor.go` — who gets transcribed. No LiveKit in it: the room hands it a
  ranking of identities and frames of audio, and it decides.
- `scribe.go` — turns into transcripts, over hanzoai/speech's growing-transcript
  API. One session at a time, and one settling at a time.
- `answer.go` — what the attendant says back, and when.
- `meet.go` — the way in, and the process.

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

## What answering has to get right

**A turn ending is not an invitation; an empty floor is.** When a turn is
transcribed the attendant has something it could say, and often somebody else is
already saying something. So it waits for the floor to be EMPTY — the floor
frees itself `rest` after the holder stops, which is the pause a person waits
out before speaking. Nothing here has a second opinion about turns.

**Being interrupted costs a syllable.** The floor fills within `notice` of
anyone starting, and the attendant is not on it — it is dropped from the room's
ranking and subscribes to nothing of its own — so the first person to make a
sound takes the floor from an empty one, and that cancels the answer mid-word.
The track comes down with it.

**Nobody answers themselves.** A turn that ends while the attendant is talking
asks for an answer that, by the time it is read, has already been given. The
attendant answers only turns it has not answered, or it would reply to its own
voice and then to that.

**Two turns while thinking are one answer.** An answer is formed from the record
at the moment it is formed, so turns that land in the meantime are IN it. The
ask is a single slot: one is as good as three.

**Saying nothing is an answer.** People in a meeting talk to each other as well
as to the attendant, and the model is told to answer with nothing when the last
thing said was not addressed to it. That is the only way it has of declining a
floor it was not offered.

## Measured

A meeting of three, then the same meeting with nine more people who came to
listen with their mics open (`TestTheFloorHoldsOneSession`):

| in the room | published | transcribed | turns |
|---|---|---|---|
| 3  | 18.6 s  | 17.0 s | ana, ben, cleo |
| 12 | 198.3 s | 17.0 s | ana, ben, cleo |

Four times the room, the same bill — **11.6× less audio decoded** than a
transcript per person, and the nine who never spoke never opened one.

## Running one

The attendant holds no LiveKit signing key. It asks cloud's meet app for one
room's token exactly as a browser does, so what it carries is one room's
capability — and meet admits a **person**, so the attendant's identity is an IAM
account like anyone else's, a member of the workspace whose room it sits in.
Rooms are named the way meet names them: `<workspace>_<room>_<id>`.

| | |
|---|---|
| `ROOM` | the room to join. Required. |
| `HANZO_TOKEN` | its IAM identity, carried to meet, the model and the voice alike. Required. |
| `MEET` | `https://api.hanzo.ai` |
| `AI` | `https://api.hanzo.ai` |
| `MODEL` | `best` |
| `SPEECH` | `http://speech.hanzo.svc` |
| `VOICE` | `af_heart` |
| `NAME` | `Attendant`, the label the room shows |
| `PROMPT` | what the model is told it is |

It opens no port and answers nothing: it is a member of a room, and when the
membership ends the process ends with it.

## Tests

    go test -race ./...          # the floor and the answer, on a clock the test drives
    ./mutate.sh                  # breaks each fix, requires its test to go red

`mutate.sh` rewrites source in place and restores it, so it is run before
pushing and never on a CI runner that builds an image from the same tree.

Live, with credentials in the environment:

    LK_URL / LK_KEY / LK_SECRET  # the real SFU: TestTheRoomAwardsTheFloor
    SPEECH                       # hanzoai/speech: TestAMeetingBecomesNotes

`fixture/make.sh` builds the multi-party fixture: three voices from
hanzoai/speech saying three different things, so attribution is checked by
content and not by hope.
