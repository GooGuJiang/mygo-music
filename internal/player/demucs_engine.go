package player

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"
)

type demucsJob struct {
	ctx      context.Context
	rate     int
	stereo   []float32
	beats    bool
	analysis bool
	result   chan demucsRender
}

type demucsRender struct {
	bed   []float32
	err   error
	stats *demucsStats
	beats []int32
}

type demucsStats struct {
	device DemucsMode
	speed  float64
}

func (job demucsJob) renderOn(worker *demucsWorker) demucsRender {
	start := time.Now()
	bed, err := worker.render(job.rate, job.stereo)
	result := demucsRender{bed: bed, err: err}
	if err == nil {
		result.stats = &demucsStats{device: worker.device}
		if elapsed := time.Since(start).Seconds(); elapsed > 0 {
			result.stats.speed = float64(len(job.stereo)/2) / float64(job.rate) / elapsed
		}
	}
	return result
}

var errDemucsStopped = errors.New("Demucs worker stopped")
var errDemucsMissing = errors.New("Demucs runtime is missing")

// PrepareDemucs prewarms the selected device on a background goroutine.
func (p *Player) PrepareDemucs() {
	mode := DemucsOff
	if p.Demucs != nil {
		mode = p.Demucs()
	}
	if p.demucs != nil {
		if p.demucs.mode == mode && !p.demucs.failed.Load() {
			return
		}
		p.demucs.release()
		p.demucs = nil
	}
	if mode == DemucsOff {
		return
	}
	if p.demucsContext == nil {
		p.demucsContext, p.cancelDemucs = context.WithCancel(context.Background())
	}
	p.demucs = newDemucsEngine(p.demucsContext, mode, func(ctx context.Context, mode DemucsMode) (*demucsWorker, error) {
		worker := findDemucs()
		if worker == "" {
			return nil, errDemucsMissing
		}
		return startDemucs(ctx, worker, mode, codecRate)
	}, func() { p.post(func() {}) })
}

// RefreshDemucs prepares the selected GPU backend for the next track after an import.
func (p *Player) RefreshDemucs() {
	if p.Demucs == nil {
		return
	}
	mode := p.Demucs()
	if mode != DemucsAuto && mode != DemucsCUDA {
		return
	}
	if p.demucs != nil {
		p.demucs.release()
		p.demucs = nil
	}
	p.PrepareDemucs()
}

// CloseDemucs shuts down both the prepared worker and any finishing its track.
func (p *Player) CloseDemucs() {
	if p.cancelDemucs != nil {
		p.cancelDemucs()
	}
	if p.demucs != nil {
		p.demucs.release()
		p.demucs = nil
	}
}

// One engine owns the pipe. A cancelled track can discard a render without
// interrupting the worker or leaving a partial response for the next track.
type demucsEngine struct {
	mode         DemucsMode
	cancel       context.CancelFunc
	changed      func()
	jobs         chan demucsJob
	analysisJobs chan demucsJob
	done         chan struct{}
	users        atomic.Int32
	device       atomic.Pointer[DemucsMode]
	failed       atomic.Bool
	rhythm       atomic.Bool
}

func newDemucsEngine(ctx context.Context, mode DemucsMode, start func(context.Context, DemucsMode) (*demucsWorker, error), changed func()) *demucsEngine {
	ctx, cancel := context.WithCancel(ctx)
	e := &demucsEngine{mode: mode, cancel: cancel, changed: changed, jobs: make(chan demucsJob), analysisJobs: make(chan demucsJob), done: make(chan struct{})}
	e.users.Store(1)
	go e.run(ctx, start)
	return e
}

func (e *demucsEngine) retain() { e.users.Add(1) }

func (e *demucsEngine) release() {
	if e.users.Add(-1) == 0 {
		e.cancel()
	}
}

func (e *demucsEngine) status() DemucsStatus {
	s := DemucsStatus{Requested: true, Failed: e.failed.Load()}
	if device := e.device.Load(); device != nil {
		s.Device = *device
	}
	return s
}

func (e *demucsEngine) render(ctx context.Context, rate int, stereo []float32) demucsRender {
	return e.request(demucsJob{ctx: ctx, rate: rate, stereo: stereo})
}

func (e *demucsEngine) request(job demucsJob) demucsRender {
	job.result = make(chan demucsRender, 1)
	ctx := job.ctx
	jobs := e.jobs
	if job.beats || job.analysis {
		jobs = e.analysisJobs
	}
	select {
	case <-ctx.Done():
		return demucsRender{err: ctx.Err()}
	case <-e.done:
		return demucsRender{err: errDemucsStopped}
	case jobs <- job:
	}
	select {
	case <-ctx.Done():
		return demucsRender{err: ctx.Err()}
	case result := <-job.result:
		return result
	}
}

func (e *demucsEngine) nextJob(ctx context.Context) (demucsJob, bool) {
	// A waiting playback block goes ahead of the next Automix analysis block.
	select {
	case job := <-e.jobs:
		return job, true
	default:
		select {
		case <-ctx.Done():
			return demucsJob{}, false
		case job := <-e.jobs:
			return job, true
		case job := <-e.analysisJobs:
			return job, true
		}
	}
}

func (e *demucsEngine) run(ctx context.Context, start func(context.Context, DemucsMode) (*demucsWorker, error)) {
	defer close(e.done)
	var worker *demucsWorker
	defer func() {
		if worker != nil {
			worker.Close()
		}
	}()
	open := func(mode DemucsMode) error {
		var err error
		worker, err = start(ctx, mode)
		if err == nil {
			e.rhythm.Store(worker.rhythm)
			e.device.Store(&worker.device)
			e.changed()
		}
		return err
	}
	fail := func(err error) {
		if ctx.Err() == nil {
			e.failed.Store(true)
			e.changed()
			log.Printf("Demucs: %v", err)
		}
	}
	err := open(e.mode)
	if err != nil && !errors.Is(err, errDemucsMissing) && e.mode == DemucsAuto && ctx.Err() == nil {
		err = open(DemucsCPU)
	}
	if err != nil {
		fail(err)
		return
	}
	for {
		job, ok := e.nextJob(ctx)
		if !ok {
			return
		}
		if job.ctx.Err() != nil || ctx.Err() != nil {
			job.result <- demucsRender{err: errDemucsStopped}
			continue
		}
		if job.beats {
			beats, err := worker.detectBeats(job.rate, job.stereo)
			job.result <- demucsRender{beats: beats, err: err}
			if err == nil {
				continue
			}
			e.rhythm.Store(false)
			e.changed()
			if errors.Is(err, errBeatUnavailable) {
				continue
			}
			worker.Close()
			if err := open(worker.device); err != nil {
				fail(err)
				return
			}
			worker.rhythm = false
			e.rhythm.Store(false)
			continue
		}
		result := job.renderOn(worker)
		if result.err != nil && e.mode == DemucsAuto && worker.device == DemucsCUDA && ctx.Err() == nil {
			e.device.Store(nil)
			e.changed()
			worker.Close()
			result = demucsRender{err: open(DemucsCPU)}
			if result.err == nil && job.ctx.Err() == nil {
				result = job.renderOn(worker)
			}
		}
		job.result <- result
		if result.err != nil {
			fail(result.err)
			return
		}
	}
}
