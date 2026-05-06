package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// jobState is the public state of the active bench job inside askd.
type jobState string

const (
	jobIdle    jobState = "idle"
	jobRunning jobState = "running"
	jobPaused  jobState = "paused"
)

// jobSpec is the JSON payload accepted by POST /bench. Fields not
// supplied are filled from the askd default options.
type jobSpec struct {
	Bench        int      `json:"bench"`
	Count        int      `json:"count,omitempty"`
	DurationMs   int      `json:"duration_ms,omitempty"`
	RampStart    int      `json:"ramp_start,omitempty"`
	RampEnd      int      `json:"ramp_end,omitempty"`
	RampDurMs    int      `json:"ramp_duration_ms,omitempty"`
	Loop         bool     `json:"loop,omitempty"`
	Random       bool     `json:"random,omitempty"`
	Warmup       bool     `json:"warmup,omitempty"`
	Files        []string `json:"files,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	URL          string   `json:"url,omitempty"`
	Token        string   `json:"token,omitempty"`
	Model        string   `json:"model,omitempty"`
	SystemPrompt string   `json:"system,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
	Stream       *bool    `json:"stream,omitempty"`
	DSL          string   `json:"dsl,omitempty"`
	Quiet        bool     `json:"quiet,omitempty"`
	JSON         bool     `json:"json,omitempty"`
	Report       *bool    `json:"report,omitempty"`
}

// jobStatus is the JSON payload returned by GET /bench.
type jobStatus struct {
	State        jobState `json:"state"`
	StartedAt    string   `json:"started_at,omitempty"`
	StoppedAt    string   `json:"stopped_at,omitempty"`
	PausedAt     string   `json:"paused_at,omitempty"`
	Spec         *jobSpec `json:"spec,omitempty"`
	LogFile      string   `json:"log_file,omitempty"`
	InflightReqs int64    `json:"inflight_requests"`
	CompletedReqs int64   `json:"completed_requests"`
	Message      string   `json:"message,omitempty"`
}

// benchController coordinates pause/stop/restart with the worker loop.
// A nil controller (CLI mode) means workers run un-gated.
type benchController struct {
	mu      sync.Mutex
	cond    *sync.Cond
	paused  atomic.Bool
	stopped atomic.Bool

	inflight  atomic.Int64
	completed atomic.Int64
}

func newBenchController() *benchController {
	c := &benchController{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// gate blocks while the controller is paused. Returns true if work
// should continue, false if ctx is done or the job is stopping.
func (c *benchController) gate(ctx context.Context) bool {
	if c == nil {
		return ctx.Err() == nil
	}
	c.mu.Lock()
	for c.paused.Load() && !c.stopped.Load() && ctx.Err() == nil {
		// Use a goroutine-friendly broadcast wait. We can't pass ctx
		// to Cond.Wait, so wake periodically to re-check ctx.
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.cond.Broadcast()
			case <-done:
			}
		}()
		c.cond.Wait()
		close(done)
	}
	c.mu.Unlock()
	if c.stopped.Load() || ctx.Err() != nil {
		return false
	}
	return true
}

func (c *benchController) setPaused(p bool) {
	if c == nil {
		return
	}
	c.paused.Store(p)
	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *benchController) setStopped() {
	if c == nil {
		return
	}
	c.stopped.Store(true)
	c.paused.Store(false)
	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
}

// waitDrain blocks until all in-flight requests finish or ctx fires.
func (c *benchController) waitDrain(ctx context.Context) {
	if c == nil {
		return
	}
	for c.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// runBenchControlled is the bench loop used by --serve. It mirrors
// runBench but: (a) takes a caller-supplied ctx (no signal handlers),
// (b) honors a benchController for pause/stop/inflight tracking.
func runBenchControlled(ctx context.Context, opts options, canonical string, stdout, stderr io.Writer, ctl *benchController) error {
	src, _, err := buildPromptSource(opts, Prompt{Text: canonical, DSL: opts.dsl})
	if err != nil {
		return err
	}

	httpClient := &http.Client{}

	if opts.warmup {
		fmt.Fprintln(stderr, "warmup: 1 untimed request")
		warmOpts := opts
		warmOpts.maxTokens = min(warmOpts.maxTokens, 32)
		p, ok := src.Next()
		if !ok {
			return fmt.Errorf("warmup: prompt source empty")
		}
		_ = sendRequest(context.Background(), httpClient, p.applyOverrides(warmOpts), p.Text, joinDSL(p.DSL, opts.dsl), 0, 0, nil, nil)
	}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.duration > 0 {
		var cancelDur context.CancelFunc
		jobCtx, cancelDur = context.WithTimeout(jobCtx, opts.duration)
		defer cancelDur()
	}

	maxWorkers := opts.bench
	rampStart := opts.rampStart
	rampEnd := opts.rampEnd
	if rampEnd == 0 {
		rampStart, rampEnd = maxWorkers, maxWorkers
	}

	results := make(chan Result, maxWorkers*2)
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(cancel) }

	var aggDone sync.WaitGroup
	aggDone.Add(1)
	go func() {
		defer aggDone.Done()
		runAggregator(opts, results, stdout)
	}()

	startBanner(stderr, opts, maxWorkers)

	var wg sync.WaitGroup
	for i := 1; i <= maxWorkers; i++ {
		wg.Add(1)
		delay := rampDelay(i, rampStart, rampEnd, opts.rampDuration)
		go func(id int, delay time.Duration) {
			defer wg.Done()
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-jobCtx.Done():
					return
				}
			}
			for {
				if !ctl.gate(jobCtx) {
					return
				}
				p, ok := src.Next()
				if !ok {
					return
				}
				ctl.inflight.Add(1)
				r := sendRequest(jobCtx, httpClient, p.applyOverrides(opts), p.Text, joinDSL(p.DSL, opts.dsl), id, -1, nil, nil)
				ctl.inflight.Add(-1)
				results <- r
				done := ctl.completed.Add(1)
				if opts.count > 0 && done >= int64(opts.count) {
					stopFn()
					return
				}
			}
		}(i, delay)
	}

	wg.Wait()
	close(results)
	aggDone.Wait()

	if ctl != nil && ctl.stopped.Load() {
		return errJobStopped
	}
	return nil
}

var errJobStopped = errors.New("job stopped")
