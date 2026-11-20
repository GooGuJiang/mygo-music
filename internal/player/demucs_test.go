package player

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
)

// Optional stem analysis cannot jump ahead of an already waiting playback block.
func TestDemucsPrioritizesWaitingPlayback(t *testing.T) {
	engine := demucsEngine{jobs: make(chan demucsJob, 1), analysisJobs: make(chan demucsJob, 1)}
	engine.analysisJobs <- demucsJob{analysis: true}
	engine.jobs <- demucsJob{}
	if job, ok := engine.nextJob(t.Context()); !ok || job.analysis {
		t.Fatal("took analysis ahead of playback")
	}
	if job, ok := engine.nextJob(t.Context()); !ok || !job.analysis {
		t.Fatal("analysis did not resume after playback")
	}
}

// Preparing starts one worker before audio arrives, and auto keeps its CPU fallback for later tracks.
func TestDemucsEngineReusesFallbackAcrossRates(t *testing.T) {
	const frames = 3
	var cpuInput bytes.Buffer
	started := make(chan DemucsMode, 4)
	e := newDemucsEngine(t.Context(), DemucsAuto, func(_ context.Context, mode DemucsMode) (*demucsWorker, error) {
		started <- mode
		if mode == DemucsAuto {
			return &demucsWorker{device: DemucsCUDA, input: io.Discard, output: bytes.NewReader(nil), Close: func() {}}, nil
		}
		output := bytes.NewReader(make([]byte, 2*frames*demucsFrameSamples*4))
		return &demucsWorker{device: DemucsCPU, input: &cpuInput, output: output, Close: func() {}}, nil
	}, func() {})
	defer e.release()

	select {
	case mode := <-started:
		if mode != DemucsAuto {
			t.Fatalf("first startup used %q", mode)
		}
	case <-t.Context().Done():
		t.Fatal("nothing prewarmed")
	}
	for _, rate := range []int{48000, 44100} {
		ctx, cancel := context.WithCancel(t.Context())
		result := e.render(ctx, rate, make([]float32, frames*2))
		cancel()
		if result.err != nil || len(result.bed) != frames*demucsFrameSamples ||
			result.stats == nil || result.stats.device != DemucsCPU || e.status().Device != DemucsCPU {
			t.Fatalf("%d Hz: bed length %d, status %+v, error %v", rate, len(result.bed), e.status(), result.err)
		}
	}
	if mode := <-started; mode != DemucsCPU || len(started) != 0 {
		t.Fatalf("expected one CPU fallback, got %q and %d more startups", mode, len(started))
	}
	if b := cpuInput.Bytes(); binary.LittleEndian.Uint32(b[4:]) != 48000 || binary.LittleEndian.Uint32(b[8+frames*8+4:]) != 44100 {
		t.Fatal("the resident worker did not receive the two different sample rates")
	}
}

// Adjusting each stem must change its front and surround contributions without moving the other stems.
func TestDemucsMixKeepsStemPlacement(t *testing.T) {
	const fold = 0.70710678
	voice := [atmosPCMChannels]float32{0.23 - fold*0.18, 0.16 - fold*0.18, 0.18}
	drums := [atmosPCMChannels]float32{
		0.11 - fold*(0.03+0.07), 0.19 - fold*(-0.02+0.04), 0, 0,
		0.03, -0.02, 0, 0, 0.07, 0.04,
	}
	bass := [atmosPCMChannels]float32{0.1, 0.14}
	other := [atmosPCMChannels]float32{0.12, 0.16, 0, 0, 0, 0, 0.05, 0.04, -0.01, 0.03}
	stems := [4][atmosPCMChannels]float32{demucsVocals: voice, demucsDrums: drums, demucsBass: bass, demucsOther: other}
	var packet [demucsFrameSamples]float32
	for _, stem := range stems {
		for ch, sample := range stem {
			packet[ch] += sample
		}
	}
	copy(packet[atmosPCMChannels:], []float32{0.23, 0.16, 0.11, 0.19, 0.1, 0.14, 0.07, 0.04})
	for i, stem := range stems {
		var gains demucsGains
		gains[i] = -0.5
		var out [atmosPCMChannels]float32
		mixDemucsFrame(out[:], packet[:], 1, gains)
		for ch, got := range out {
			if want := packet[ch] - 0.5*stem[ch]; math.Abs(float64(got-want)) > 1e-6 {
				t.Errorf("stem %d, channel %d: got %g, want %g", i, ch, got, want)
			}
		}
	}
}

// The direct mix preserves the original stereo at unity and applies stem gains without using spatial sends.
func TestDemucsStereoMixPreservesChannelsAndResidual(t *testing.T) {
	original := []float32{0.6, -0.4}
	var packet [demucsFrameSamples]float32
	packet[2], packet[4], packet[8] = 0.9, -0.9, 0.9
	copy(packet[atmosPCMChannels:], []float32{0.2, -0.1, 0.1, 0.2, 0.1, -0.1})
	out := [2]float32{original[0], original[1]}
	mixDemucsStereo(out[:], packet[:], 1, demucsGains{})
	if out != [2]float32{original[0], original[1]} {
		t.Fatalf("unity mix = %v, want %v", out, original)
	}
	mixDemucsStereo(out[:], packet[:], 1, demucsGains{demucsVocals: -0.5, demucsOther: -0.25})
	if math.Abs(float64(out[0]-0.45)) > 1e-6 || math.Abs(float64(out[1]+0.25)) > 1e-6 {
		t.Fatalf("adjusted mix = %v, want [0.45 -0.25]", out)
	}
}

// Late blocks must fade to the original stereo, and a seek must discard the old bed.
func TestDemucsCrossfadesMissingBlocksAndDiscardsSeek(t *testing.T) {
	const fade = 48000 * dolbyFadeMillis / 1000
	dry := make([]float32, 12*fade)
	for i := 0; i < len(dry); i += 2 {
		dry[i], dry[i+1] = 0.2, -0.2
	}
	s := &demucsStream{stream: &dolbyTestStream{samples: dry}, results: make(chan demucsBlock, 2), wake: make(chan struct{}, 1)}
	req := &demucsRequest{}
	s.request.Store(req)
	payload := make([]float32, 4*fade*demucsFrameSamples)
	for i := 0; i < len(payload); i += demucsFrameSamples {
		payload[i], payload[i+1], payload[i+8] = 0.4, -0.4, 0.3
	}
	s.results <- demucsBlock{request: req, start: fade, bed: payload}
	stereo := make([]float32, len(dry))
	bed := make([]float32, len(dry)/2*atmosPCMChannels)
	weights := make([]float32, len(dry)/2)
	if n, err := s.readSurround(stereo, bed, weights); n != len(dry) || err != io.EOF {
		t.Fatalf("read = %d, %v", n, err)
	}
	for frame, want := range map[int]float32{0: 0, fade - 1: 0, 3 * fade: 0.3, 5*fade - 1: 0, 5 * fade: 0} {
		if got := bed[frame*atmosPCMChannels+8]; got != want {
			t.Errorf("height sample at %d = %g, want %g", frame, got, want)
		}
	}
	for i := atmosPCMChannels; i < len(bed); i += atmosPCMChannels {
		if step := bed[i+8] - bed[i+8-atmosPCMChannels]; step < -0.001 || step > 0.001 {
			t.Fatalf("height signal jumped by %g at frame %d", step, i/atmosPCMChannels)
		}
	}
	if err := s.SetPosition(0); err != nil {
		t.Fatal(err)
	}
	s.results <- demucsBlock{request: req, bed: payload}
	s.readSurround(stereo[:2*fade], bed[:fade*atmosPCMChannels], weights[:fade])
	for i := 0; i < fade*atmosPCMChannels; i += atmosPCMChannels {
		if bed[i] != 0.2 || bed[i+1] != -0.2 || bed[i+8] != 0 {
			t.Fatal("a model block from before the seek reached the new position")
		}
	}
}
