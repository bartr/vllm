package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// runBench is the bench-mode entry point.
func runBench(opts options, canonical string, stdout, stderr io.Writer) error {
	src, _, err := buildPromptSource(opts, Prompt{Text: canonical, DSL: opts.dsl})
	if err != nil {
		return err
	}

	httpClient := &http.Client{}

	// Optional warmup.
	if opts.warmup {
		fmt.Fprintln(stderr, "warmup: 1 untimed request")
		warmOpts := opts
		warmOpts.maxTokens = min(warmOpts.maxTokens, 32)
		p, ok := src.Next()
		if !ok {
			// One-shot prompt source can't be exhausted, but a tiny
			// file might be: refuse rather than skip silently.
			return fmt.Errorf("warmup: prompt source empty")
		}
		_ = sendRequest(context.Background(), httpClient, p.applyOverrides(warmOpts), p.Text, joinDSL(p.DSL, opts.dsl), 0, 0, nil, nil)
	}

	// Stop conditions: Ctrl-C, optional --duration, optional --count.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.duration)
		defer cancel()
	}

	// Concurrency shape.
	maxWorkers := opts.bench
	rampStart := opts.rampStart
	rampEnd := opts.rampEnd
	if rampEnd == 0 {
		// No ramp: all workers start immediately.
		rampStart, rampEnd = maxWorkers, maxWorkers
	}

	// Channels.
	results := make(chan Result, maxWorkers*2)
	var completed atomic.Int64
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(stop) }

	// Live tail printer.
	var aggDone sync.WaitGroup
	aggDone.Add(1)
	go func() {
		defer aggDone.Done()
		runAggregator(opts, results, stdout)
	}()

	startBanner(stderr, opts, maxWorkers)

	// Workers.
	var wg sync.WaitGroup
	startTime := time.Now()
	for i := 1; i <= maxWorkers; i++ {
		wg.Add(1)
		delay := rampDelay(i, rampStart, rampEnd, opts.rampDuration)
		go func(id int, delay time.Duration) {
			defer wg.Done()
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			for {
				if ctx.Err() != nil {
					return
				}
				p, ok := src.Next()
				if !ok {
					return
				}
				r := sendRequest(ctx, httpClient, p.applyOverrides(opts), p.Text, joinDSL(p.DSL, opts.dsl), id, -1, nil, nil)
				results <- r
				if opts.count > 0 && completed.Add(1) >= int64(opts.count) {
					stopFn()
					return
				}
			}
		}(i, delay)
	}

	wg.Wait()
	close(results)
	aggDone.Wait()
	_ = startTime // start time tracked by aggregator via per-result timestamps
	return nil
}

// joinDSL combines a per-prompt DSL string with the request-level --dsl
// flag. Both are optional. The flag is appended after the per-prompt DSL
// so per-request directives can override file-level ones (the parser is
// first-wins-per-class, so per-prompt wins on class conflicts; otherwise
// flag tokens fill in).
func joinDSL(promptDSL, flagDSL string) string {
	switch {
	case promptDSL == "" && flagDSL == "":
		return ""
	case promptDSL == "":
		return flagDSL
	case flagDSL == "":
		return promptDSL
	default:
		return promptDSL + " " + flagDSL
	}
}

// rampDelay computes when worker `i` (1-based) should start. With no
// ramp, all workers start immediately. With a ramp, workers are spread
// linearly across rampDuration: the rampStart-th worker starts at t=0
// and the rampEnd-th starts at t=rampDuration.
func rampDelay(i, rampStart, rampEnd int, rampDuration time.Duration) time.Duration {
	if rampDuration <= 0 || rampEnd == rampStart {
		return 0
	}
	if i <= rampStart {
		return 0
	}
	// Linear: worker rampStart at 0, worker rampEnd at rampDuration.
	frac := float64(i-rampStart) / float64(rampEnd-rampStart)
	return time.Duration(frac * float64(rampDuration))
}

func startBanner(w io.Writer, opts options, maxWorkers int) {
	fmt.Fprintf(w, "target: %s\n", opts.url)
	fmt.Fprintf(w, "model: %s\n", opts.model)
	fmt.Fprintf(w, "concurrency: %d\n", maxWorkers)
	if opts.rampEnd > 0 {
		fmt.Fprintf(w, "ramp: %d -> %d over %s\n", opts.rampStart, opts.rampEnd, opts.rampDuration)
	}
	if opts.duration > 0 {
		fmt.Fprintf(w, "duration: %s\n", opts.duration)
	}
	if opts.count > 0 {
		fmt.Fprintf(w, "count: %d\n", opts.count)
	}
	if opts.loop {
		fmt.Fprintln(w, "loop: forever")
	}
	if len(opts.files) > 0 {
		fmt.Fprintf(w, "files: %v (random=%t)\n", opts.files, opts.random)
	}
	fmt.Fprintln(w, "press Ctrl-C to stop")
}
