// The way in.
//
// The attendant holds no LiveKit signing key. It asks cloud's meet app for one
// room's token exactly as a browser does — the lobby for where the SFU is, then
// the token itself — so what it carries is one room's capability, and the key
// that could open every room in the deployment stays where it already is.
//
// It carries ONE identity, from Hanzo IAM, and carries it to all of them: meet,
// the model, and the voice. Nothing here mints anything.
//
// meet admits a PERSON. It wants a subject and a privileged membership in the
// room's workspace, and refuses a machine credential by name — so the
// attendant's identity is an IAM account like anyone else's, a member of the
// workspace it sits in, and the room it joins is named the way meet names
// rooms: <workspace>_<room>_<id>.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

// What the model is told it is.
//
// It answers into a room where people talk to each other as well as to it, so
// the first thing it needs is permission to say nothing: no words at all is how
// it stays out of a conversation that is not with it, and the only way it has
// of not taking a floor it was not offered.
const brief = "You are the attendant, a participant in a spoken meeting. " +
	"You are given the transcript so far, one line per turn, with names on them. " +
	"Answer the last thing said, in two sentences at most, plainly, as speech: " +
	"no lists, no markdown, no stage directions. " +
	"If the last thing said was not addressed to you, answer with nothing at all."

// reach is the one client the attendant calls anything with. `patience` bounds
// every call for the reason it bounds an answer: a call that never returns is a
// meeting that never hears from the attendant again.
var reach = &http.Client{Timeout: patience}

// ask sends one request with the attendant's identity on it and answers with
// the body.
func ask(ctx context.Context, key, method, url, mime string, body []byte) ([]byte, error) {
	var in io.Reader
	if body != nil {
		in = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, in)
	if err != nil {
		return nil, err
	}
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := reach.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, url, res.Status, bytes.TrimSpace(got))
	}
	return got, nil
}

// seat is what meet's lobby says: who the caller is in a room, and where the
// rooms are. The identity is meet's to decide and not ours to choose, which is
// why it is read back rather than configured.
type seat struct {
	Identity string `json:"identity"`
	WS       string `json:"ws"`
}

func lobby(ctx context.Context, meet, key string) (seat, error) {
	got, err := ask(ctx, key, http.MethodGet, meet+"/v1/meet/session", "", nil)
	if err != nil {
		return seat{}, err
	}
	var here seat
	if err := json.Unmarshal(got, &here); err != nil {
		return seat{}, err
	}
	return here, nil
}

// ticket is the join token for one room. It answers in text, not JSON, because
// that is what meet answers with — the token is the whole body.
func ticket(ctx context.Context, meet, key, room, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"roomName": room, "participantName": name})
	got, err := ask(ctx, key, http.MethodPost, meet+"/v1/meet/getToken", "application/json", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(got)), nil
}

// env is a setting with a default. Everything past the room and the identity
// has one.
func env(name, or string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return or
}

func main() {
	room, key := env("ROOM", ""), env("HANZO_TOKEN", "")
	if room == "" || key == "" {
		log.Fatal("ROOM and HANZO_TOKEN are required")
	}
	meet := env("MEET", "https://api.hanzo.ai")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	here, err := lobby(ctx, meet, key)
	if err != nil {
		log.Fatalf("meet: %v", err)
	}
	if here.WS == "" {
		log.Fatal("meet names no SFU: LIVEKIT_WS is unset where meet runs")
	}
	token, err := ticket(ctx, meet, key, room, env("NAME", "Attendant"))
	if err != nil {
		log.Fatalf("meet: %v", err)
	}

	speech := env("SPEECH", "http://speech.hanzo.svc")
	notes := Record(speech, "whisper", "en")
	say := &Answer{
		Notes:  notes,
		ai:     env("AI", "https://api.hanzo.ai"),
		model:  env("MODEL", "best"),
		speech: speech,
		voice:  env("VOICE", "af_heart"),
		prompt: env("PROMPT", brief),
		who:    here.Identity,
		key:    key,
		wake:   make(chan struct{}, 1),
	}

	floor := Chair(ctx, say)
	att, err := Attend(ctx, here.WS, token, floor)
	if err != nil {
		log.Fatalf("join %s: %v", room, err)
	}
	defer att.Leave()
	log.Printf("attending %s as %s", room, here.Identity)

	go adjourn(ctx, att, stop)
	say.tend(ctx, floor, att.Say)

	if err := floor.Close(); err != nil {
		log.Printf("close: %v", err)
	}
	fmt.Print(notes.Text())
}

// adjourn ends the process when the membership does. A join token is good for ten
// minutes and this one is spent, so the way back into a room we have fallen out
// of is a new process with a new token — not this one sitting in a room it can
// no longer hear.
func adjourn(ctx context.Context, a *Attendant, gone func()) {
	t := time.NewTicker(beat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if a.room.ConnectionState() == lksdk.ConnectionStateDisconnected {
				log.Print("the room is gone")
				gone()
				return
			}
		}
	}
}
