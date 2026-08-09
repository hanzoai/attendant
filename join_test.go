package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// The LiveKit join token, built exactly as apps/meet builds it (meet.go grant):
// HS256, iss = api key, sub = identity, and a video grant narrowed to one room.
func mint(key, secret, room, identity string, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"iss":  key,
		"sub":  identity,
		"iat":  now.Unix(),
		"nbf":  now.Unix(),
		"exp":  now.Add(ttl).Unix(),
		"name": identity,
		"video": map[string]any{
			"roomJoin": true, "room": room, "canSubscribe": true, "canPublish": true,
		},
	})
	claims := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + claims))
	return header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type collector struct {
	mu       sync.Mutex
	samples  int
	speakers map[string]bool
}

func (c *collector) Hear(speaker string, pcm []int16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.speakers == nil {
		c.speakers = map[string]bool{}
	}
	c.speakers[speaker] = true
	c.samples += len(pcm)
}

// Joins the real SFU. Proves membership, not compilation: it reads back the room
// name and the identity the server assigned, then leaves.
func TestAttendJoinsLiveRoom(t *testing.T) {
	key, secret, url := os.Getenv("LK_KEY"), os.Getenv("LK_SECRET"), os.Getenv("LK_URL")
	if key == "" || secret == "" || url == "" {
		t.Skip("no LiveKit credentials in env")
	}
	room := fmt.Sprintf("probe%d", time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := &collector{}
	a, err := Attend(ctx, url, mint(key, secret, room, "attendant", 5*time.Minute), got)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer a.Leave()

	if a.room.Name() != room {
		t.Fatalf("joined %q, wanted %q", a.room.Name(), room)
	}
	t.Logf("JOINED room=%q as identity=%q sid=%s",
		a.room.Name(), a.room.LocalParticipant.Identity(), a.room.LocalParticipant.SID())

	// A second membership in the same room, so the attendant has someone to see.
	other, err := Attend(ctx, url, mint(key, secret, room, "human", 5*time.Minute), &collector{})
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	defer other.Leave()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.room.GetRemoteParticipants()) > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	peers := a.room.GetRemoteParticipants()
	if len(peers) == 0 {
		t.Fatalf("attendant never saw the other participant")
	}
	for _, p := range peers {
		t.Logf("SEES participant identity=%q sid=%s", p.Identity(), p.SID())
	}
}

// Leave must be safe twice: ctx cancellation and an explicit call race by design.
func TestLeaveIsIdempotent(t *testing.T) {
	key, secret, url := os.Getenv("LK_KEY"), os.Getenv("LK_SECRET"), os.Getenv("LK_URL")
	if key == "" || secret == "" || url == "" {
		t.Skip("no LiveKit credentials in env")
	}
	room := fmt.Sprintf("probe%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())

	a, err := Attend(ctx, url, mint(key, secret, room, "attendant", 5*time.Minute), &collector{})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	a.Leave()
	a.Leave() // must not panic
	cancel()
	time.Sleep(500 * time.Millisecond)
	t.Log("left twice and cancelled, no panic")
}
