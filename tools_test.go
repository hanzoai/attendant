// Copyright 2026 Hanzo AI, Inc. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// estate stands in for api.hanzo.ai: it reports what the org can do, runs what is
// asked, and answers as the model. One host, because that is how the real one is
// reached.
type estate struct {
	mu    sync.Mutex
	ran   []string          // tools actually dispatched, in order
	sent  []json.RawMessage // the arguments each was given
	turns int               // completions served
	tools string            // the /v1/tools body
	reply []string          // one completion body per turn
}

func (e *estate) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/tools":
			io.WriteString(w, e.tools)
		case r.URL.Path == "/v1/tools/call":
			var in struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"arguments"`
			}
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &in)
			e.ran = append(e.ran, in.Name)
			e.sent = append(e.sent, in.Args)
			io.WriteString(w, `{"name":"todo_create","content":"ENG-41 created"}`)
		case r.URL.Path == "/v1/chat/completions":
			i := e.turns
			e.turns++
			if i < len(e.reply) {
				io.WriteString(w, e.reply[i])
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
		}
	}))
}

const oneTool = `{"tools":[{"name":"todo_create","description":"File an item",
"inputSchema":{"type":"object","properties":{"title":{"type":"string"}}},
"dispatchable":true,"activated":true}]}`

// The point of the whole file: a meeting that decides something leaves the
// decision somewhere a person will see it, without a person copying it.
func TestWhatTheMeetingDecidedIsFiled(t *testing.T) {
	e := &estate{tools: oneTool, reply: []string{
		`{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function",
		  "function":{"name":"todo_create","arguments":"{\"title\":\"ship the relay fix\"}"}}]}}]}`,
		`{"choices":[{"message":{"content":"Filed ENG-41."}}]}`,
	}}
	srv := e.serve()
	defer srv.Close()

	a := answering(Record(srv.URL, "whisper", "en"), srv.URL)
	said, err := a.reply(context.Background(), "we agreed to ship the relay fix")
	if err != nil {
		t.Fatal(err)
	}
	if said != "Filed ENG-41." {
		t.Errorf("the room heard %q, not the answer that followed the filing", said)
	}
	if len(e.ran) != 1 || e.ran[0] != "todo_create" {
		t.Fatalf("tools dispatched = %v, want one todo_create", e.ran)
	}
	if !strings.Contains(string(e.sent[0]), "ship the relay fix") {
		t.Errorf("the item was filed without what was decided: %s", e.sent[0])
	}
}

// A tool the org has not turned on is not offered. Offering one earns a refusal
// the room hears as the attendant being broken.
func TestOnlyWhatTheOrgTurnedOnIsOffered(t *testing.T) {
	e := &estate{tools: `{"tools":[
	 {"name":"on","description":"","inputSchema":{"type":"object"},"dispatchable":true,"activated":true},
	 {"name":"off","description":"","inputSchema":{"type":"object"},"dispatchable":true,"activated":false},
	 {"name":"undispatchable","description":"","inputSchema":{"type":"object"},"dispatchable":false,"activated":true}]}`}
	srv := e.serve()
	defer srv.Close()

	got := offered(context.Background(), srv.URL, "")
	if len(got) != 1 {
		t.Fatalf("offered %d tools, want only the activated dispatchable one", len(got))
	}
	fn := got[0]["function"].(map[string]any)
	if fn["name"] != "on" {
		t.Errorf("offered %q", fn["name"])
	}
}

// An attendant that cannot read the catalog still talks. Losing the follow-up is
// a smaller failure than losing the meeting.
func TestAnUnreadableCatalogStillAnswers(t *testing.T) {
	e := &estate{tools: `not json`}
	srv := e.serve()
	defer srv.Close()

	a := answering(Record(srv.URL, "whisper", "en"), srv.URL)
	said, err := a.reply(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if said != "done" {
		t.Errorf("said %q — the answer did not survive an unreadable catalog", said)
	}
}

// A model that keeps calling tools is a model that is not answering, and the room
// is listening to silence. The last round is asked without them so the turn ends
// in words.
func TestTheLoopEndsInWordsNotAnotherCall(t *testing.T) {
	call := `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c","type":"function",
	  "function":{"name":"todo_create","arguments":"{}"}}]}}]}`
	e := &estate{tools: oneTool, reply: []string{call, call, call, call, call}}
	srv := e.serve()
	defer srv.Close()

	a := answering(Record(srv.URL, "whisper", "en"), srv.URL)
	if _, err := a.reply(context.Background(), "loop"); err != nil {
		t.Fatal(err)
	}
	if e.turns > rounds {
		t.Errorf("asked the model %d times, bound is %d", e.turns, rounds)
	}
}
