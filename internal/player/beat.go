package player

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"time"
)

const beatSpan = 10 * time.Second
const automixWindow = 24 * time.Second // maximum beat request accepted by the worker

// MetronomeMaxVolume is four times the nominal click level.
const MetronomeMaxVolume = 4

var errBeatUnavailable = errors.New("Beat This is unavailable")

type beatEvent struct {
	at  time.Duration
	bar bool
}

type beatTimeline struct {
	start, end time.Duration
	events     []beatEvent
	tones      []tonalFrame
	energy     []automixEnergy
	stems      []automixStem
}

func (w *demucsWorker) detectBeats(rate int, stereo []float32) ([]int32, error) {
	if !w.rhythm {
		return nil, errBeatUnavailable
	}
	if w.abort != nil {
		defer time.AfterFunc(15*time.Second, w.abort).Stop()
	}
	if err := w.writePCM(uint32(len(stereo)/2)|1<<31, rate, stereo); err != nil {
		return nil, err
	}
	var count int32
	if err := binary.Read(w.output, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	if count == -1 {
		w.rhythm = false
		return nil, errBeatUnavailable
	}
	if count < 0 || count > 1200 {
		return nil, errors.New("invalid Beat This event count")
	}
	events := make([]int32, count)
	return events, binary.Read(w.output, binary.LittleEndian, events)
}

func detectBeats(ctx context.Context, engine *demucsEngine, s stream, start, span time.Duration) (*beatTimeline, []float32, error) {
	rate := s.SampleRate()
	if s.Channels() != 2 || rate < 32000 || rate > 192000 || !engine.rhythm.Load() {
		return nil, nil, errBeatUnavailable
	}
	frame := start.Nanoseconds() * int64(rate) / int64(time.Second)
	if err := s.SetPosition(frame); err != nil {
		return nil, nil, err
	}
	pcm := make([]float32, 2*rate*int(span/time.Second))
	count := 0
	for count < len(pcm) && ctx.Err() == nil {
		n, err := s.Read(pcm[count:])
		count += n
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		if n == 0 || err == io.EOF {
			break
		}
	}
	pcm = pcm[:count-count%2]
	timeline := &beatTimeline{start: start, end: start + time.Duration(len(pcm)/2)*time.Second/time.Duration(rate)}
	if len(pcm) < 2*rate {
		return timeline, nil, ctx.Err()
	}
	result := engine.request(demucsJob{ctx: ctx, rate: rate, stereo: pcm, beats: true})
	if result.err != nil {
		return nil, nil, result.err
	}
	for _, x := range result.beats {
		bar := x < 0
		if bar {
			x = -x
		}
		at := start + time.Duration(x-1)*20*time.Millisecond
		if at >= start && at < timeline.end {
			timeline.events = append(timeline.events, beatEvent{at, bar})
		}
	}
	return timeline, pcm, nil
}

func (s *demucsStream) analyzeRhythm(ctx context.Context, duration int64) {
	defer s.engine.release()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	engine := s.engine
	for engine.device.Load() == nil {
		select {
		case <-ctx.Done():
			return
		case <-engine.done:
			return
		case <-tick.C:
		}
	}
	if !engine.rhythm.Load() {
		return
	}
	dl, _ := s.source()
	decoder, err := openShared(dl, duration)
	if err != nil {
		return
	}
	defer decoder.Close()
	rate := time.Duration(s.SampleRate())
	last := time.Duration(-1)
	lastTail := false
	for {
		at := time.Duration(s.position.Load()) * time.Second / rate
		start := max(0, (at+2*time.Second).Truncate(beatSpan)-4*time.Second)
		old := s.rhythm.Load()
		tail := false
		if frames := decoder.Len(); frames > 0 {
			end := time.Duration(frames) * time.Second / rate
			tailStart := max(0, end-automixWindow)
			if end-at <= automixWindow+4*time.Second && (at >= tailStart || old != nil && old.start <= at && old.end > tailStart+3*time.Second) {
				start, tail = tailStart, true
			}
		}
		if (start != last || tail != lastTail) &&
			(decoder.Len() < 0 || start < time.Duration(decoder.Len())*time.Second/rate) {
			span := 18 * time.Second
			if tail {
				span = automixWindow
			}
			window, _, err := detectBeats(ctx, engine, decoder, start, span)
			if err != nil {
				return
			}
			if old != nil && start > last && start+3*time.Second < old.end {
				cut := start + 3*time.Second
				keep := start - 4*time.Second
				if tail {
					keep = start - automixWindow // keep preceding downbeats for the tempo ramp
				}
				events := slices.DeleteFunc(slices.Clone(old.events), func(event beatEvent) bool {
					return event.at < keep || event.at >= cut
				})
				for _, event := range window.events {
					if event.at >= cut && (len(events) == 0 || event.at-events[len(events)-1].at >= 180*time.Millisecond) {
						events = append(events, event)
					}
				}
				window.start, window.events = max(old.start, keep), events
			}
			s.rhythm.Store(window)
			engine.changed()
			last, lastTail = start, tail
		}
		select {
		case <-ctx.Done():
			return
		case <-engine.done:
			return
		case <-tick.C:
		}
	}
}

func (t *beatTimeline) bpm(at time.Duration) int {
	if t == nil || at < t.start || at >= t.end {
		return 0
	}
	var intervals []time.Duration
	for i := 1; i < len(t.events); i++ {
		event := t.events[i].at
		d := event - t.events[i-1].at
		if event >= at-5*time.Second && event <= at+3*time.Second && d >= 250*time.Millisecond && d <= 1200*time.Millisecond {
			intervals = append(intervals, d)
		}
	}
	if len(intervals) < 3 {
		return 0
	}
	slices.Sort(intervals)
	median := intervals[len(intervals)/2]
	if intervals[len(intervals)*3/4]-intervals[len(intervals)/4] > median/7 {
		return 0
	}
	return int(math.Round(60 / median.Seconds()))
}

// BPM reports the local tempo near the audio being heard.
func (p *Player) BPM() int {
	if s, ok := p.stream.(*demucsStream); ok {
		return s.rhythm.Load().bpm(p.Position())
	}
	return 0
}

func (p *Player) Metronome() bool { return p.metronome }

func (p *Player) SetMetronome(on bool) {
	p.metronome = on
	p.applyMetronome()
}

func (p *Player) MetronomeVolume() float64 { return p.metronomeVolume }

func (p *Player) SetMetronomeVolume(volume float64) {
	p.metronomeVolume = min(MetronomeMaxVolume, max(0, volume))
	p.applyMetronome()
}

func (p *Player) applyMetronome() {
	if p.watch != nil {
		var gain float64
		if p.metronome {
			gain = p.metronomeVolume
		}
		p.watch.clickGain.Store(math.Float64bits(gain))
	}
}

func (t *beatTimeline) addClicks(pcm []float32, frame int64, rate int, volume float64) {
	if t == nil {
		return
	}
	length := int64(rate) * 55 / 1000
	attack := float64(rate) * 0.002
	for _, event := range t.events {
		beat := event.at.Nanoseconds() * int64(rate) / int64(time.Second)
		first, last := max(frame, beat), min(frame+int64(len(pcm)/2), beat+length)
		if first >= last {
			continue
		}
		frequency, gain := 1600.0, 0.19
		if event.bar {
			frequency, gain = 2350, 0.24
		}
		gain *= volume
		for f := first; f < last; f++ {
			offset := float64(f - beat)
			mix := float32(gain * min(1, offset/attack) * (1 - offset/float64(length)))
			click := mix * float32(math.Sin(2*math.Pi*frequency*offset/float64(rate)))
			i := 2 * (f - frame)
			pcm[i] = pcm[i]*(1-mix) + click
			pcm[i+1] = pcm[i+1]*(1-mix) + click
		}
	}
}
