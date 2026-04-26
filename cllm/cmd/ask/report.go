package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// runAggregator consumes Result values, prints a live tail line per
// result (unless --json, which emits NDJSON), tracks a 15-second sliding
// window for instantaneous tokens/sec, and prints a final summary on
// channel close. It is meant to be run in its own goroutine.
func runAggregator(opts options, results <-chan Result, stdout io.Writer) {
	var collected []Result
	const windowSeconds = 15
	window := windowDeque{maxAge: windowSeconds * time.Second}

	if !opts.json {
		fmt.Fprintf(stdout, "\n%-7s %-7s %-9s %-12s %-11s %-12s %-6s\n",
			"thread", "tokens", "ttft_ms", "duration_ms", "req_tok/s", "total_tok/s", "cache")
	}

	for r := range results {
		collected = append(collected, r)

		if opts.json {
			_ = writeResultJSON(stdout, r)
			continue
		}

		ttft := "n/a"
		if r.TTFT >= 0 {
			ttft = fmt.Sprintf("%.2f", float64(r.TTFT.Microseconds())/1000.0)
		}
		duration := fmt.Sprintf("%.2f", float64(r.Duration().Microseconds())/1000.0)
		reqTPS := 0.0
		if r.Duration() > 0 && r.OK {
			reqTPS = float64(r.CompletionTokens) / r.Duration().Seconds()
		}

		if r.OK {
			window.add(r.StartedAt, r.EndedAt, r.CompletionTokens)
		}
		totalTPS := window.tps(r.EndedAt)

		cache := ""
		if r.OK {
			cache = "miss"
			if r.CacheHit {
				cache = "hit"
			}
		}

		fmt.Fprintf(stdout, "%-7d %-7d %-9s %-12s %-11.2f %-12.2f %-6s",
			r.Worker, r.CompletionTokens, ttft, duration, reqTPS, totalTPS, cache)
		if !r.OK {
			fmt.Fprintf(stdout, "  ERR: %s", trimErr(r.Err, 80))
		}
		fmt.Fprintln(stdout)
	}

	if opts.report && !opts.json {
		printReport(stdout, collected)
	}
}

// windowDeque tracks (started_at, ended_at, completion_tokens) tuples
// within a rolling time window for sliding tokens/sec. A request is
// kept while any part of it overlaps the window, so a single long
// request never collapses the rate to 0. The reported coverage is
// clamped to the window so a long-running request's tps reflects its
// actual decode rate rather than the elapsed wall clock.
type windowDeque struct {
	maxAge time.Duration
	items  []windowItem
}

type windowItem struct {
	start  time.Time
	end    time.Time
	tokens int
}

func (w *windowDeque) add(start, end time.Time, tokens int) {
	w.items = append(w.items, windowItem{start: start, end: end, tokens: tokens})
}

func (w *windowDeque) tps(now time.Time) float64 {
	cutoff := now.Add(-w.maxAge)
	// Evict only requests that ended before the window started.
	for len(w.items) > 0 && w.items[0].end.Before(cutoff) {
		w.items = w.items[1:]
	}
	if len(w.items) == 0 {
		return 0
	}
	var sum int
	for _, it := range w.items {
		sum += it.tokens
	}
	// Coverage is from the earliest in-window start to now, clamped to maxAge.
	earliest := w.items[0].start
	if earliest.Before(cutoff) {
		earliest = cutoff
	}
	covered := now.Sub(earliest)
	if covered <= 0 {
		return 0
	}
	return float64(sum) / covered.Seconds()
}

func trimErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// printReport emits the final P50/P95/P99 + throughput + cache summary.
func printReport(w io.Writer, results []Result) {
	if len(results) == 0 {
		fmt.Fprintln(w, "\n-- report --\nno requests completed")
		return
	}
	var (
		oks         []Result
		errs        int
		statusHist  = map[int]int{}
		cacheHits   int
		startMin    = results[0].StartedAt
		endMax      = results[0].EndedAt
		totalTokens int
	)
	for _, r := range results {
		if r.StartedAt.Before(startMin) {
			startMin = r.StartedAt
		}
		if r.EndedAt.After(endMax) {
			endMax = r.EndedAt
		}
		if r.OK {
			oks = append(oks, r)
			totalTokens += r.CompletionTokens
			if r.CacheHit {
				cacheHits++
			}
		} else {
			errs++
			if r.StatusCode > 0 {
				statusHist[r.StatusCode]++
			}
		}
	}
	wall := endMax.Sub(startMin)

	fmt.Fprintln(w, "\n-- report --")
	fmt.Fprintf(w, "wall_clock:        %s\n", wall.Round(time.Millisecond))
	fmt.Fprintf(w, "requests:          %d (ok=%d, err=%d)\n", len(results), len(oks), errs)
	if errs > 0 {
		for code, n := range statusHist {
			fmt.Fprintf(w, "  status %d: %d\n", code, n)
		}
	}
	if wall > 0 {
		fmt.Fprintf(w, "throughput:        %.2f tok/s\n",
			float64(totalTokens)/wall.Seconds())
	}
	fmt.Fprintf(w, "cache_hits:        %d/%d (%.1f%%)\n", cacheHits, len(oks), pct(cacheHits, len(oks)))

	if len(oks) == 0 {
		return
	}

	ttfts := make([]float64, 0, len(oks))
	for _, r := range oks {
		if r.TTFT >= 0 {
			ttfts = append(ttfts, float64(r.TTFT.Microseconds())/1000.0)
		}
	}
	durations := make([]float64, len(oks))
	decodeTPS := make([]float64, 0, len(oks))
	for i, r := range oks {
		durations[i] = float64(r.Duration().Microseconds()) / 1000.0
		if v := r.DecodeTPS(); v > 0 {
			decodeTPS = append(decodeTPS, v)
		}
	}

	if len(ttfts) > 0 {
		printQuantiles(w, "ttft_ms", ttfts)
	}
	printQuantiles(w, "duration_ms", durations)
	if len(decodeTPS) > 0 {
		printQuantiles(w, "decode_tok/s", decodeTPS)
	}
}

func printQuantiles(w io.Writer, label string, xs []float64) {
	sort.Float64s(xs)
	fmt.Fprintf(w, "%-18s min=%.2f  p50=%.2f  p95=%.2f  p99=%.2f  max=%.2f  n=%d\n",
		label+":",
		xs[0],
		quantile(xs, 0.50),
		quantile(xs, 0.95),
		quantile(xs, 0.99),
		xs[len(xs)-1],
		len(xs),
	)
}

// quantile returns a linear-interpolated quantile of a pre-sorted slice.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func pct(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return 100 * float64(num) / float64(den)
}

