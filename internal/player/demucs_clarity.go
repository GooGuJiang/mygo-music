package player

import "math"

type clarityBand struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
}

type demucsClarity struct {
	bands         [4]clarityBand
	step, release float64
	mix, gain     float64
}

func newDemucsClarity(rate int) *demucsClarity {
	eq := &demucsClarity{
		step:    1 / float64(max(1, rate*dolbyFadeMillis/1000)),
		release: 1 - math.Exp(-1/(0.05*float64(rate))),
		gain:    1,
	}
	frequencies := [...]float64{3750, 5813, 9000, 11250}
	gains := [...]float64{1, 2, 2, 1}
	for i, hz := range frequencies {
		// Peaking filter from the Audio EQ Cookbook, with Q = 1.5.
		sine, cosine := math.Sincos(2 * math.Pi * hz / float64(rate))
		alpha := sine / 3
		a := math.Pow(10, gains[i]/40)
		a0 := 1 + alpha/a
		eq.bands[i] = clarityBand{
			b0: (1 + alpha*a) / a0,
			b1: -2 * cosine / a0,
			b2: (1 - alpha*a) / a0,
			a1: -2 * cosine / a0,
			a2: (1 - alpha/a) / a0,
		}
	}
	return eq
}

func (eq *demucsClarity) reset() {
	if eq.mix == 0 {
		return
	}
	eq.mix, eq.gain = 0, 1
	for i := range eq.bands {
		clear(eq.bands[i].z1[:])
		clear(eq.bands[i].z2[:])
	}
}

func (eq *demucsClarity) process(samples, weights []float32, enabled bool) {
	const (
		trim    = 0.7943282347 // -2 dB of headroom for the boosted bands
		ceiling = 0.8413951416 // -1.5 dBFS leaves room for peaks between samples
	)
	for i, wet := range weights {
		if wet == 0 {
			eq.reset()
			continue
		}
		if enabled {
			eq.mix = min(1, eq.mix+eq.step)
		} else if eq.mix <= eq.step {
			eq.reset()
			return
		} else {
			eq.mix -= eq.step
		}
		frame := samples[2*i : 2*i+2]
		y := [2]float64{float64(frame[0]), float64(frame[1])}
		for b := range eq.bands {
			f := &eq.bands[b]
			for ch, x := range y {
				out := f.b0*x + f.z1[ch]
				f.z1[ch] = f.b1*x - f.a1*out + f.z2[ch]
				f.z2[ch] = f.b2*x - f.a2*out
				y[ch] = out
			}
		}
		// Limit the filtered branch with one gain for both channels to preserve the stereo image.
		eq.gain += (1 - eq.gain) * eq.release
		peak := trim * max(math.Abs(y[0]), math.Abs(y[1]))
		if peak > ceiling {
			eq.gain = min(eq.gain, ceiling/peak)
		}
		mix := eq.mix * eq.mix * (3 - 2*eq.mix) * float64(wet)
		for ch, x := range frame {
			frame[ch] = float32(float64(x) + mix*(trim*eq.gain*y[ch]-float64(x)))
		}
	}
}
