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
	bands      automixBands
	frame      int64
}

func (w *watched) setAutomix(plan *automixPlan, incoming bool) {
	var eq *automixEQ
	var stems *automixStemEnvelope
	if plan != nil {
		rate := w.SampleRate()
		eq = newAutomixEQ(plan, rate, incoming)
		if w.rhythm != nil {
			stems = newAutomixStemEnvelope(plan, rate, incoming)
			eq.stemMix = plan.stems.analyzed || plan.bridge
		}
	}
	if w.rhythm != nil {
		w.rhythm.automix.Store(stems)
	}
	w.automix.Store(eq)
}

func (w *watched) Read(dst []float32) (int, error) {
	defer w.begin()()
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
