// A server-side participant for a LiveKit room: it joins, subscribes to the
// audio every human publishes, and hands 16 kHz mono PCM to a listener.
//
// One attendant per room, summoned on demand and dismissed by cancelling its
// context — never a process-wide singleton, because two rooms share nothing:
// not a decoder (an Opus decoder carries per-stream state), not a transcript,
// and not a tenant.
//
// Audio arrives as Opus at 48 kHz, which is what WebRTC carries. It leaves as
// 16 kHz mono int16, which is the only shape hanzoai/speech accepts. The
// decoder is asked for that rate directly rather than decoding at 48 kHz and
// resampling afterwards: one step, and no resampler of ours to be wrong.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Rate and channels hanzoai/speech requires (stt.RATE, mono). Not configurable:
// the service refuses anything else by name rather than resampling it.
const (
	rate     = 16000
	channels = 1
)

// Speech is what an attendant does with the audio it hears. One call per
// decoded frame, in arrival order, per speaker.
type Speech interface {
	Hear(speaker string, pcm []int16)
}

// Attendant is one room membership. Dismiss by cancelling the context passed to
// Attend, or by calling Leave; both are safe to do twice.
type Attendant struct {
	room  *lksdk.Room
	heard Speech
	left  *dismissal
}

// dismissal is a one-shot that can be tripped by a context OR by a call, runs its
// effect exactly once, and retires its watch either way.
//
// It is separate from the room because it has nothing to do with the room, and
// because the bug it fixes is not a LiveKit behaviour: a bare `go func(){
// <-ctx.Done(); leave() }` parks forever when the caller dismisses by calling
// Leave on a context nobody cancels — which is the documented way to dismiss an
// attendant. One parked goroutine per meeting, for the life of the process.
//
// The effect is a func rather than a method on Attendant so that it is bound
// AFTER the join returns a room. Nothing can then observe a half-built attendant,
// and there is no nil room to guard against.
type dismissal struct {
	once sync.Once
	quit chan struct{}
	then func()
}

// dismissed starts the watch: `then` runs when ctx is cancelled, or when dismiss
// is called, whichever happens first, and never twice.
func dismissed(ctx context.Context, then func()) *dismissal {
	d := &dismissal{quit: make(chan struct{}), then: then}
	go func() {
		select {
		case <-ctx.Done():
			d.dismiss()
		case <-d.quit:
		}
	}()
	return d
}

func (d *dismissal) dismiss() {
	d.once.Do(func() {
		close(d.quit)
		d.then()
	})
}

// Attend joins room as identity and stays until ctx is cancelled.
//
// The token is minted by the caller, not here: minting needs the LiveKit signing
// key, and an attendant that could mint its own could join any room in the
// deployment. It is handed exactly one room's capability.
func Attend(ctx context.Context, url, token string, heard Speech) (*Attendant, error) {
	if heard == nil {
		return nil, errors.New("attend: no listener")
	}
	a := &Attendant{heard: heard}

	room, err := lksdk.ConnectToRoomWithToken(url, token, &lksdk.RoomCallback{
		OnActiveSpeakersChanged: a.speaking,
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: a.listen,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("attend: join: %w", err)
	}
	a.room = room
	a.left = dismissed(ctx, room.Disconnect)
	return a, nil
}

// speaking passes the room's active speakers, loudest first, to a listener that
// asked to know. One that did not is unaffected — it still hears every track.
//
// The attendant itself is dropped from the list. It is a member of the room and
// LiveKit reports it like any other, but it publishes no track we subscribe to,
// so a floor awarded to it would be a floor nothing could feed. Identified by
// type rather than by SID: this can be called during the join, before the room
// has been assigned.
func (a *Attendant) speaking(active []lksdk.Participant) {
	to, ok := a.heard.(interface{ Speaking(loud []string) })
	if !ok {
		return
	}
	loud := make([]string, 0, len(active))
	for _, p := range active {
		if _, mine := p.(*lksdk.LocalParticipant); mine {
			continue
		}
		loud = append(loud, p.Identity())
	}
	to.Speaking(loud)
}

// listen decodes one remote audio track for as long as it is published.
//
// The decoder is per-track and lives here rather than on the Attendant: Opus
// carries state across frames, so two speakers decoded through one decoder
// corrupt each other. A track that ends returns from ReadRTP and the goroutine
// retires with it.
func (a *Attendant) listen(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	go func() {
		dec, err := opus.NewDecoderWithOutput(rate, channels)
		if err != nil {
			return
		}
		// 120 ms at 48 kHz is the largest sample count an Opus packet can carry;
		// at 16 kHz out it is smaller still, so this cannot be overrun.
		out := make([]int16, 5760)
		speaker := rp.Identity()

		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return // track ended, or the room went away
			}
			if len(pkt.Payload) == 0 {
				continue
			}
			n, err := dec.DecodeToInt16(pkt.Payload, out)
			if err != nil {
				// A single undecodable packet is loss, not a reason to stop
				// listening to the speaker.
				continue
			}
			a.heard.Hear(speaker, out[:n])
		}
	}()
}

// Leave disconnects. Idempotent, so ctx cancellation and an explicit call cannot
// double-disconnect — and it holds no lock across the disconnect, so one slow
// disconnect does not stall every other room's dismissal.
func (a *Attendant) Leave() { a.left.dismiss() }

// Say publishes audio into the room and returns when the room has heard all of
// it — or the moment ctx is cancelled, which is how the attendant is cut off
// mid-sentence. The track comes down either way: one left published is a
// microphone nobody takes down, and every answer would add another.
//
// The bytes must already be Opus in an Ogg container, which is what
// hanzoai/speech returns for response_format "opus" — so nothing here encodes,
// and there is no encoder to keep honest. They are read as they are sent, so
// there is no file of ours on the way in or a temporary one to clean up.
func (a *Attendant) Say(ctx context.Context, ogg io.ReadCloser) error {
	spoken := make(chan struct{})
	track, err := lksdk.NewLocalReaderTrack(ogg, webrtc.MimeTypeOpus,
		lksdk.ReaderTrackWithOnWriteComplete(func() { close(spoken) }))
	if err != nil {
		return fmt.Errorf("attend: speech: %w", err)
	}
	pub, err := a.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		return fmt.Errorf("attend: publish: %w", err)
	}
	defer a.room.LocalParticipant.UnpublishTrack(pub.SID())

	select {
	case <-spoken:
	case <-ctx.Done():
	}
	return nil
}
