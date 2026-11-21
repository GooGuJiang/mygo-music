package player

import (
	"math"
	"sync/atomic"
	"time"
)

// starvedAfter is how long the playing stream may wait for its download before
// the player says it's buffering: decoding takes milliseconds, and it's asked
// for more only as there's room for it, so a call this long waits on bytes
// that haven't come in, after a seek past them, or from a network slower
// than the song.
const starvedAfter = 250 * time.Millisecond

// watched is the stream that plays, timing what it's asked.
type watched struct {
	stream
	now func() time.Time
	// ended hears that a call that took starvedAfter or longer returned,
	// for what said it's buffering to say so no more.
	ended      func()
	firstRead  func(bool) // decoder goroutine, once the cue has PCM or has failed
	firstFrame int64
	since      atomic.Int64 // when the call on its way began, in Unix nanoseconds, or 0
	rhythm     *demucsStream
	clickGain  atomic.Uint64 // the click's float64 gain, or zero while off
	automix    atomic.Pointer[automixEQ]
	tempo      atomic.Pointer[automixTempo]
	bands      automixBands
	stretch    *automixStretch // decoder goroutine only
	frame      int64
}

func (w *watched) setAutomix(plan *automixPlan, incoming bool) {
	var eq *automixEQ
	var stems *automixStemEnvelope
	var loop *automixLoopAudio
	if plan != nil {
		if plan.preparedAudio(false) != nil {
			if prepared := plan.preparedAudio(incoming); prepared != nil && prepared.rate == w.SampleRate() {
				loop = prepared
			}
		} else {
			rate := w.SampleRate()
			eq = newAutomixEQ(plan, rate, incoming)
			if w.rhythm != nil {
				stems = newAutomixStemEnvelope(plan, rate, incoming)
				eq.stemMix = plan.stems.analyzed || plan.bridge
			}
		}
	}
	if w.rhythm != nil {
		w.rhythm.automix.Store(stems)
		w.rhythm.automixLoop.Store(loop)
	}
	w.automix.Store(eq)
}

func (w *watched) Read(dst []float32) (int, error) {
	defer w.begin()()
	if w.stretch != nil {
		return w.stretch.read(w, dst)
	}
	if tempo := w.tempo.Load(); tempo != nil {
		switch {
		case w.frame < tempo.first:
			return w.readPCM(dst[:min(int64(len(dst)), 2*(tempo.first-w.frame))])
		case w.frame == tempo.first:
			tempo.started.Store(true)
			if api := loadRubberBand(); api != nil && w.tempo.Load() == tempo {
				if state := api.open(uint32(w.SampleRate())); state != 0 {
					rate := w.SampleRate()
					w.stretch = &automixStretch{api: api, state: state, tempo: tempo, rate: rate, output: w.frame, seam: make([]float32, 0, 2*(rate/20))}
					return w.stretch.read(w, dst)
				}
			}
		}
		tempo.failed.Store(true)
		w.tempo.CompareAndSwap(tempo, nil)
	}
	return w.readPCM(dst)
}

func (w *watched) readPCM(dst []float32) (int, error) {
	n, err := w.stream.Read(dst)
	frame := w.frame
	if eq := w.automix.Load(); eq != nil && n > 0 {
		w.bands.apply(dst[:n], frame, w.SampleRate(), eq)
	}
	if w.rhythm != nil && n > 0 {
		if gain := math.Float64frombits(w.clickGain.Load()); gain > 0 {
			w.rhythm.rhythm.Load().addClicks(dst[:n], frame, w.SampleRate(), gain)
		}
	}
	if n > 0 {
		w.frame += int64(n / w.Channels())
	}
	if first := w.firstRead; first != nil && frame >= w.firstFrame && (n > 0 || err != nil) {
		w.firstRead = nil
		first(n > 0)
	}
	return n, err
}

func (w *watched) SetPosition(frame int64) error {
	defer w.begin()()
	w.closeStretch()
	if err := w.stream.SetPosition(frame); err != nil {
		if first := w.firstRead; first != nil && frame >= w.firstFrame {
			w.firstRead = nil
			first(false)
		}
		return err
	}
	w.frame = frame
	w.bands = automixBands{}
	return nil
}

func (w *watched) closeStretch() {
	if w.stretch != nil {
		w.stretch.api.close(w.stretch.state)
		w.stretch = nil
	}
}

func (w *watched) Close() error {
	w.closeStretch()
	return w.stream.Close()
}

func (w *watched) sourceTime(at time.Duration) time.Duration {
	if w != nil {
		return w.tempo.Load().mapTime(at, false)
	}
	return at
}

func (w *watched) begin() func() {
	start := w.now()
	w.since.Store(start.UnixNano())
	return func() {
		w.since.Store(0)
		if w.ended != nil && w.now().Sub(start) >= starvedAfter {
			w.ended()
		}
	}
}

// starved reports whether the call on its way has waited starvedAfter.
func (w *watched) starved() bool {
	since := w.since.Load()
	return since != 0 && w.now().UnixNano()-since >= int64(starvedAfter)
}
