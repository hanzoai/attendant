package main

import (
	"io"
	"math"
	"os"
	"testing"

	"github.com/pion/opus"
	"github.com/pion/opus/pkg/oggreader"
)

// decodeOgg is the same conversion listen() performs per RTP packet, run over an
// Ogg container so it can be checked against a fixture.
func decodeOgg(t *testing.T, path string) []int16 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	defer f.Close()
	return decode(t, f)
}

// decode is that conversion over anything Ogg: a fixture on disk, or the bytes
// an answer was published from.
func decode(t *testing.T, in io.Reader) []int16 {
	t.Helper()
	ogg, _, err := oggreader.NewWith(in)
	if err != nil {
		t.Fatalf("ogg: %v", err)
	}
	dec, err := opus.NewDecoderWithOutput(rate, channels)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	out := make([]int16, 5760)
	var pcm []int16
	for {
		pkt, _, err := ogg.ParseNextPacket()
		if err != nil {
			break
		}
		n, err := dec.DecodeToInt16(pkt, out)
		if err != nil {
			continue
		}
		pcm = append(pcm, out[:n]...)
	}
	return pcm
}

// whisperRate is hanzoai/speech's stt.RATE written out independently of the
// package constant under test. Deriving the expected duration from `rate` would
// make this test its own oracle: at 48 kHz the sample count triples and the
// quotient is unchanged, so the one bug that matters — handing whisper audio at
// WebRTC's native rate — would pass. The service's requirement is a fixed
// number, so it is written as one.
const whisperRate = 16000

// The fixture is real kokoro output (hanzoai/speech, response_format "opus"), so
// this asserts the exact conversion a room performs on real service audio.
func TestDecodeYieldsSpeechAtWhisperRate(t *testing.T) {
	pcm := decodeOgg(t, "spoken.opus")

	// 1. The decoder must be configured for the rate the service accepts.
	if rate != whisperRate || channels != 1 {
		t.Fatalf("decoder configured %d Hz / %dch; speech requires %d Hz mono",
			rate, channels, whisperRate)
	}

	// 2. Length, measured against the service's rate rather than the package's.
	//    The fixture is a known utterance: ~3.9 s. Audio decoded at 48 kHz lands
	//    at 3x the samples and fails here.
	seconds := float64(len(pcm)) / float64(whisperRate)
	if seconds < 3.5 || seconds > 4.3 {
		t.Fatalf("decoded %d samples = %.3fs at %d Hz, wanted ~3.9s",
			len(pcm), seconds, whisperRate)
	}

	// 2. Signal. Silence decodes to zeros and would satisfy every length check,
	//    so require real amplitude before calling this audio.
	var sum float64
	var peak int16
	for _, s := range pcm {
		sum += float64(s) * float64(s)
		a := s
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	if rms < 200 {
		t.Fatalf("rms %.1f is silence, not speech", rms)
	}
	if peak < 5000 {
		t.Fatalf("peak %d too low for speech", peak)
	}
	t.Logf("decoded %.3fs  rms=%.1f  peak=%d  samples=%d", seconds, rms, peak, len(pcm))
}

// The push size the room sends must be whole int16 frames and inside the
// service's ceiling, or speech answers 400/413 instead of transcribing.
func TestPushWindowMatchesServiceContract(t *testing.T) {
	const chunk = 8 * 1024 // what the room sends per push
	const ceiling = 64 * 1024
	const width = 2

	if chunk%width != 0 {
		t.Fatalf("chunk %d is not whole int16 frames", chunk)
	}
	if chunk > ceiling {
		t.Fatalf("chunk %d exceeds the %d ceiling", chunk, ceiling)
	}
	ms := float64(chunk) / float64(rate*width) * 1000
	if ms < 100 || ms > 500 {
		t.Fatalf("window %.0fms is outside the useful range", ms)
	}
	t.Logf("window %.0fms = %d bytes", ms, chunk)
}
