// Copyright 2026 Hanzo AI, Inc. All rights reserved.
// SPDX-License-Identifier: MIT

// What the attendant can DO about what it heard.
//
// Answering out loud is the whole of a conversation and none of the follow-up. A
// meeting that decides something leaves the decision in the transcript, where it
// stays until a person copies it somewhere — and the copying is the part that
// does not happen.
//
// The tools are not named here. api.hanzo.ai already reports what the caller's
// org offers (GET /v1/tools) and runs them (POST /v1/tools/call), so the
// attendant offers whatever that answer holds and nothing it does not: an org
// that has todo gets todo_create, one that has more gets more, and this file
// never learns which. Hardcoding a tool would put the catalog in two places and
// make the second one wrong the first time either moved.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// rounds bounds the tool loop. A model that keeps calling tools is a model that
// is not answering, and a room waiting on it hears silence — so the loop is
// short and the last word is always spoken, not called.
const rounds = 3

// kit is what this org can do, already in the shape the model is asked with.
type kit []map[string]any

// offered reads the caller's own tools. A failure is not fatal: an attendant
// that cannot list tools is an attendant that only talks, which is what it did
// before this file existed.
func offered(ctx context.Context, ai, key string) kit {
	got, err := ask(ctx, key, http.MethodGet, ai+"/v1/tools", "", nil)
	if err != nil {
		return nil
	}
	var listed struct {
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			Schema       json.RawMessage `json:"inputSchema"`
			Dispatchable bool            `json:"dispatchable"`
			Activated    bool            `json:"activated"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got, &listed); err != nil {
		return nil
	}
	var out kit
	for _, t := range listed.Tools {
		// Dispatchable AND activated: the registry reports everything the org
		// could have, and offering one it has not turned on earns a refusal the
		// room would hear as the attendant being broken.
		if !t.Dispatchable || !t.Activated || len(t.Schema) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Schema,
			},
		})
	}
	return out
}

// call runs one tool the model asked for and returns what to tell it back.
//
// An error is returned AS THE RESULT rather than raised: the model asked a
// question and a sentence saying why it cannot be answered is something it can
// act on, where an aborted turn just loses the meeting's follow-up silently.
func call(ctx context.Context, ai, key, name string, args json.RawMessage) string {
	// `arguments` arrives as a JSON-encoded STRING, not an object — the model
	// writes the call's input as text and the transport carries it as text. Read
	// it as an object first anyway: a provider that sends the object directly is
	// not wrong, and one shape must not be the only one that works.
	var parsed map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			var text string
			if err := json.Unmarshal(args, &text); err != nil {
				return fmt.Sprintf("could not read the arguments for %s: %v", name, err)
			}
			if text = strings.TrimSpace(text); text != "" {
				if err := json.Unmarshal([]byte(text), &parsed); err != nil {
					return fmt.Sprintf("could not read the arguments for %s: %v", name, err)
				}
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"name": name, "arguments": parsed})
	got, err := ask(ctx, key, http.MethodPost, ai+"/v1/tools/call", "application/json", body)
	if err != nil {
		return fmt.Sprintf("%s did not run: %v", name, err)
	}
	return strings.TrimSpace(string(got))
}
