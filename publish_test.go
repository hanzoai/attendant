package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// A participant that says its lines into a real room, so the live test drives
// the SFU the way a browser does.
//
// The audio level a publisher stamps on its packets is what livekit's observer
// ranks speakers by — the SDK stamps a fixed one on a file track, so it is set
// here from the loudness of the audio itself. That keeps the live room and the
// replayed one ranking the same speech the same way.

// dBov is the level a publisher would stamp for this audio: RFC 6464 carries
// -dBov relative to full scale, so 0 is as loud as it gets and 127 is silence.
func dBov(pcm []int16) uint8 {
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	if len(pcm) == 0 || sum == 0 {
		return 127
	}
	q := -20 * math.Log10(math.Sqrt(sum/float64(len(pcm)))/32768)
	switch {
	case q < 0:
		return 0
	case q > 127:
		return 127
	}
	return uint8(q)
}

// level sets the audio level on a file track. ReaderSampleProviderOption is the
// SDK's own option type, so this needs nothing the SDK does not already offer.
func level(q uint8) lksdk.ReaderSampleProviderOption {
	return func(p *lksdk.ReaderSampleProvider) { p.AudioLevel = q }
}

// publish joins as one participant and says its lines at their times, holding
// the mic only while talking — a gated mic, which is what a client does.
func publish(ctx context.Context, url, token string, lines []said) error {
	room, err := lksdk.ConnectToRoomWithToken(url, token, &lksdk.RoomCallback{})
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer room.Disconnect()

	// Nobody talks the instant they walk in, and the attendant is already in the
	// room when they do: publishing and subscribing take about a second to
	// negotiate, and audio published before that arrives nowhere. Measured — the
	// first run of this script lost ana's opening words that way, and the floor
	// cannot hand a turn audio that never reached the process.
	began := time.Now().Add(2 * time.Second)
	for _, line := range lines {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(began.Add(line.at))):
		}

		done := make(chan struct{})
		track, err := lksdk.NewLocalFileTrack(line.file,
			lksdk.ReaderTrackWithOnWriteComplete(func() { close(done) }),
			level(dBov(line.pcm)))
		if err != nil {
			return fmt.Errorf("read %s: %w", line.file, err)
		}
		pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
			Source: livekit.TrackSource_MICROPHONE,
		})
		if err != nil {
			return fmt.Errorf("publish %s: %w", line.file, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
		room.LocalParticipant.UnpublishTrack(pub.SID())
	}
	return nil
}
