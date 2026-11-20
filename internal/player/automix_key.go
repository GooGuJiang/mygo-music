package player

import (
	"context"
	"math"
	"math/cmplx"
	"time"
)

type tonalFrame struct {
	at     time.Duration
	chroma [12]float64
}

// analyzeTones folds spectral peaks into pitch classes, keeping their timestamps for cue selection.
func analyzeTones(ctx context.Context, pcm []float32, rate int, start time.Duration) []tonalFrame {
	const size = 4096
	stride := max(1, rate/8000)
	span := size * stride
	hzPerBin := float64(rate) / float64(span)
	first, last := int(math.Ceil(55/hzPerBin)), int(2000/hzPerBin)
	window := make([]float64, size)
	for i := range window {
		window[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/(size-1))
	}
	spectrum := make([]complex128, size)
	magnitudes := make([]float64, last+2)
	var frames []tonalFrame
	for offset := 0; offset+span <= len(pcm)/2 && ctx.Err() == nil; offset += span / 2 {
		for i, weight := range window {
			var mono float64
			for j := range stride {
				k := 2 * (offset + i*stride + j)
				mono += float64(pcm[k]) + float64(pcm[k+1])
			}
			spectrum[i] = complex(mono*weight/float64(2*stride), 0)
		}
		fft(spectrum)
		var peak float64
		for k := first - 1; k <= last+1; k++ {
			magnitudes[k] = cmplx.Abs(spectrum[k])
			peak = max(peak, magnitudes[k])
		}
		if !(peak >= .05 && peak < math.Inf(1)) {
			continue
		}
		frame := tonalFrame{at: start + time.Duration(offset+span/2)*time.Second/time.Duration(rate)}
		var sum float64
		for k := first; k <= last; k++ {
			left, center, right := magnitudes[k-1], magnitudes[k], magnitudes[k+1]
			if center < .08*peak || center <= left || center <= right {
				continue
			}
			bin := float64(k) + .5*(left-right)/(left-2*center+right)
			midi := int(math.Round(69 + 12*math.Log2(bin*hzPerBin/440)))
			frame.chroma[midi%12] += center
			sum += center
		}
		if sum > 0 {
			for pitch := range frame.chroma {
				frame.chroma[pitch] /= sum
			}
			frames = append(frames, frame)
		}
	}
	return frames
}

// Keys are indexed from C. Zero means the excerpt did not give a clear result.
type musicalKey uint8

func (t *beatTimeline) keyBetween(start, end time.Duration) musicalKey {
	var chroma [12]float64
	count := 0
	for _, frame := range t.tones {
		if frame.at >= start && frame.at < end {
			for pitch, weight := range frame.chroma {
				chroma[pitch] += weight
			}
			count++
		}
	}
	if count < 2 {
		return 0
	}
	return estimateKey(chroma)
}

// estimateKey uses the Krumhansl–Schmuckler correlation across the 24 major/minor keys.
func estimateKey(chroma [12]float64) musicalKey {
	profiles := [2][12]float64{
		{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88},
		{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17},
	}
	center := func(p *[12]float64) float64 {
		var sum, squares float64
		for _, x := range p {
			sum += x
		}
		for i := range p {
			p[i] -= sum / 12
			squares += p[i] * p[i]
		}
		return math.Sqrt(squares)
	}
	norm := center(&chroma)
	if norm < 1e-9 {
		return 0
	}
	best, second := -1.0, -1.0
	var key musicalKey
	for mode, profile := range profiles {
		denominator := norm * center(&profile)
		for root := range 12 {
			var score float64
			for pitch, weight := range profile {
				score += weight * chroma[(root+pitch)%12]
			}
			score /= denominator
			if score > best {
				key, best, second = musicalKey(1+mode*12+root), score, best
			} else {
				second = max(second, score)
			}
		}
	}
	if best < .55 || best-second < .035 {
		return 0
	}
	return key
}

func harmonicMatch(out, in musicalKey) int {
	if out == 0 || in == 0 {
		return 0
	}
	if out == in {
		return 2
	}
	outRoot, inRoot := int(out-1)%12, int(in-1)%12
	distance := (inRoot - outRoot + 12) % 12
	if (out <= 12) == (in <= 12) {
		if distance == 5 || distance == 7 {
			return 1
		}
	} else if out <= 12 && distance == 9 || out > 12 && distance == 3 {
		return 1
	}
	return -1
}
