package player

import (
	"io"
	"math"
	"testing"
	"time"
)

// A coherent panned source is narrow, while independent left and right signals are wide.
func TestDemucsStereoImage(t *testing.T) {
	var narrow, wide stereoMoments
	for i := range 64 {
		x := float32(2*(i%2) - 1)
		narrow.add(x, x/2)
		wide.add(x, float32(2*(i/2%2)-1))
	}
	pan, width, confidence := narrow.image(0.01)
	if pan != -0.6 || width != 0 || confidence != 1 {
		t.Fatalf("panned source: pan %g, width %g, confidence %g", pan, width, confidence)
	}
	pan, width, confidence = wide.image(0.01)
	if pan != 0 || width != 1 || confidence != 1 {
		t.Fatalf("diffuse source: pan %g, width %g, confidence %g", pan, width, confidence)
	}
}

// A deferred handoff is released once even if the incoming decoder immediately reaches EOF.
func TestWatchedSignalsFirstReadOnce(t *testing.T) {
	calls := 0
	w := &watched{stream: &dolbyTestStream{}, now: time.Now, firstRead: func(buffered bool) {
		calls++
		if buffered {
			t.Error("reported ready PCM at EOF")
		}
	}}
	if n, err := w.Read(make([]float32, 4)); n != 0 || err != io.EOF || calls != 1 {
		t.Fatalf("initial EOF: read %d, %v, callbacks %d", n, err, calls)
	}
	w.Read(make([]float32, 4))
	if calls != 1 {
		t.Fatalf("repeated EOF signalled %d callbacks", calls)
	}
}

// Adaptive sends preserve the stereo fold-down, including when the user edits individual stem gains.
func TestDemucsPlacementPreservesFoldDown(t *testing.T) {
	packet := [demucsFrameSamples]float32{0.10, 0.07, 0.18, 0, 0.03, -0.02, 0.02, 0.09, 0.07, 0.04, 0.23, 0.16, 0.11, 0.19, 0.10, 0.14, 0.04, 0.02}
	var placement demucsPlacement
	for i := range placement {
		placement[i] = 0.2 * float32(i%3-1)
	}
	gains := demucsGains{demucsVocals: 0.2, demucsDrums: -0.4, demucsBass: 0.3, demucsOther: -0.25}
	base := [atmosPCMChannels]float32{0.2, -0.12}
	mixDemucsFrame(base[:], packet[:], 0.75, gains)
	spatial := base
	addDemucsPlacement(spatial[:], packet[:], 0.75, gains, &placement)
	if spatial == base {
		t.Fatal("the placement did not change the bed")
	}
	for ch := range 2 {
		fold := func(x [atmosPCMChannels]float32) float32 {
			return x[ch] + demucsFoldGain*(x[2]+x[4+ch]+x[6+ch]+x[8+ch])
		}
		if delta := math.Abs(float64(fold(spatial) - fold(base))); delta > 1e-6 {
			t.Fatalf("channel %d fold-down changed by %g", ch, delta)
		}
	}
}
