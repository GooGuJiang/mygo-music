//go:build windows && amd64

package player

import (
	"context"
	"io"
	"math"
	"os"
	"slices"
	"testing"
	"time"
)

// The native worker provides usable beat grids, vocal activity, and drum accents to the same Automix plan.
func TestNativeAutomixPair(t *testing.T) {
	mode := DemucsMode(os.Getenv("MM_DEMUCS_NATIVE"))
	if mode != DemucsCPU && mode != DemucsCUDA {
		t.Skip("set MM_DEMUCS_NATIVE to cpu or cuda after preparing the Beat This runtime")
	}
	changed := make(chan struct{}, 1)
	engine := newDemucsEngine(t.Context(), mode, func(ctx context.Context, mode DemucsMode) (*demucsWorker, error) {
		return startDemucs(ctx, findDemucs(), mode, codecRate)
	}, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	t.Cleanup(engine.release)
	select {
	case <-changed:
	case <-t.Context().Done():
		t.Fatal("native engine did not start")
	}
	if !engine.rhythm.Load() {
		t.Fatal("native engine has no Beat This support")
	}
	dl, err := openFile("../../website/public/haruhikage.m4a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dl.Close)
	out, err := openShared(dl, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := openShared(dl, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var pair, partial *automixPair
	analyzeAutomixPair(t.Context(), engine, out, in, automixWindow, func(result *automixPair) {
		pair = result
		if partial == nil && len(result.out.stems) > 0 {
			partial = result
		}
	})
	if pair == nil || !pair.stemsReady || len(pair.out.stems) == 0 || len(pair.in.stems) == 0 {
		t.Fatal("native analysis did not publish both separated excerpts")
	}
	if !slices.ContainsFunc(pair.out.stems, func(frame automixStem) bool { return frame.vocal > .5 }) ||
		!slices.ContainsFunc(pair.in.stems, func(frame automixStem) bool { return frame.drums > .01 }) {
		t.Fatal("the music fixture did not produce distinct vocals and drums")
	}
	if partial == nil || partial.stemsReady || partial.out.stems[0].known || !partial.out.stems[len(partial.out.stems)-1].known ||
		!partial.in.stems[0].known || partial.in.stems[len(partial.in.stems)-1].known {
		t.Fatal("partial analysis did not publish the outgoing tail and incoming head first")
	}
	if plan := matchBars(pair.out, pair.in, automixWindow, 0); plan.count == 0 || plan.beats == 0 {
		t.Fatal("the native beat grids did not yield a playable overlap")
	}
}

// Beat This detects stable timing on actual music and shares its worker with separation on either device.
func TestNativeBeatThis(t *testing.T) {
	mode := DemucsMode(os.Getenv("MM_DEMUCS_NATIVE"))
	if mode != DemucsCUDA && mode != DemucsCPU {
		t.Skip("set MM_DEMUCS_NATIVE to cpu or cuda after preparing the Beat This runtime")
	}
	w, err := startDemucs(t.Context(), findDemucs(), mode, codecRate)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	dl, err := openFile("../../website/public/haruhikage.m4a")
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStream(dl, 0)
	if err != nil {
		dl.Close()
		t.Fatal(err)
	}
	defer s.Close()
	rate := s.SampleRate()
	pcm := make([]float32, int(automixWindow/time.Second)*rate*2)
	for filled := 0; filled < len(pcm); {
		n, err := s.Read(pcm[filled:])
		if n == 0 || err != nil && err != io.EOF {
			t.Fatalf("decoded only %d samples: %v", filled, err)
		}
		filled += n
	}
	start := time.Now()
	events, err := w.detectBeats(rate, pcm)
	t.Logf("first %s beat window: %s", mode, time.Since(start))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 10 || !slices.ContainsFunc(events, func(event int32) bool { return event < 0 }) {
		t.Fatalf("missing music beats or downbeats: %v", events)
	}
	start = time.Now()
	if again, err := w.detectBeats(rate, pcm); err != nil || !slices.Equal(again, events) {
		t.Fatalf("repeat after session reuse: %v, %v", again, err)
	}
	t.Logf("warm %s beat window: %s for %s of music", mode, time.Since(start), automixWindow)
	if bed, err := w.render(rate, pcm[:4*rate]); err != nil || len(bed) != 2*rate*demucsFrameSamples {
		t.Fatalf("separation after rhythm analysis: %d samples, %v", len(bed), err)
	}
}

// The native worker keeps its session across blocks at different sample rates and preserves the stereo fold-down.
func TestNativeDemucsWorker(t *testing.T) {
	mode := DemucsMode(os.Getenv("MM_DEMUCS_NATIVE"))
	if mode == DemucsOff {
		t.Skip("set MM_DEMUCS_NATIVE to cpu or cuda after preparing the Demucs bundle")
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	path := findDemucs()
	if path == "" {
		t.Fatal("Demucs worker or model missing")
	}
	worker, err := startDemucs(t.Context(), path, mode, codecRate)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if worker.device != mode {
		t.Fatalf("got device %s, want %s", worker.device, mode)
	}
	for _, rate := range []int{48000, 44100, 96000} {
		frames := 2*rate + rate/4
		stereo := make([]float32, 2*frames)
		for i := range frames {
			x := float64(i) / float64(rate)
			env := math.Exp(-8 * math.Mod(x, 0.25))
			stereo[2*i] = float32(0.15 * env * math.Sin(2*math.Pi*220*x))
			stereo[2*i+1] = float32(0.1 * env * math.Sin(2*math.Pi*440*x))
		}
		start := time.Now()
		bed, err := worker.render(rate, stereo)
		if err != nil || len(bed) != frames*demucsFrameSamples {
			t.Fatalf("%d Hz: received %d samples, error %v", rate, len(bed), err)
		}
		t.Logf("%s rendered a 2.25 s block at %d Hz in %s", mode, rate, time.Since(start))
		peak := 0.0
		for i := 64; i < frames-64; i++ {
			samples := bed[i*demucsFrameSamples:]
			for ch := range 2 {
				fold := samples[ch] + demucsFoldGain*(samples[2]+samples[4+ch]+samples[6+ch]+samples[8+ch])
				want := 0.94 * stereo[2*i+ch]
				if delta := math.Abs(float64(fold - want)); !(delta < 0.002) {
					t.Fatalf("%d Hz frame %d channel %d: fold-down %g, want %g", rate, i, ch, fold, want)
				}
				peak = max(peak, math.Abs(float64(samples[10+ch]))+math.Abs(float64(samples[12+ch]))+math.Abs(float64(samples[14+ch])))
			}
		}
		if !(peak > 0) {
			t.Fatalf("%d Hz: missing stem signal", rate)
		}
	}
}
