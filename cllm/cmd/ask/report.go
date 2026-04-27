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
		totalTPS, totalReady := window.tps(r.EndedAt)
		totalCol := ""
		if totalReady {
			totalCol = fmt.Sprintf("%.2f", totalTPS)
		}

		cache := ""
		if r.OK {
			cache = "miss"
			if r.CacheHit {
				cache = "hit"
			}
		}

		fmt.Fprintf(stdout, "%-7d %-7d %-9s %-12s %-11.2f %-12s %-6s",
			r.Worker, r.CompletionTokens, ttft, duration, reqTPS, totalCol, cache)
		if !r.OK {
			fmt.Fprintf(stdout, "  ERR: %s", trimErr(r.Err, 80))
		}
		fmt.Fprintln(stdout)
	}

	if opts.report && !opts.json {
		printReport(stdout, collected)
	}
}

// windowDeque tracks tokens/sec over a rolling window by distributing
// each completed request's token count linearly across its [start, end]
// lifetime into per-second buckets. A request that ran 8 seconds and
// produced 256 tokens contributes 32 tokens to each of the 8 seconds it
// was alive, regardless of when the aggregator actually observed its
// completion. This collapses the arrival-noise variance of clustered
// retirements: a flock of completions arriving in the same second adds
// their tokens to the seconds they were *generated*, not to the second
// they happened to be observed.
//
// Because the aggregator only observes requests at completion, the most
// recent ~one-request-lifetime of buckets is always partial (in-flight
// requests haven't back-filled them yet). To remove the resulting
// steady-state bias, the read is **lagged** by a decaying maximum of
// observed request lifetimes (a bucket is only fully filled once the
// longest possibly-overlapping request has retired). The lag is
// clamped to maxAge - 1 so coverage never collapses to zero.
type windowDeque struct {
	maxAge  time.Duration
	buckets map[int64]float64 // unix-second -> tokens generated in that second
	first   time.Time         // start of the first observed request (warm-up anchor)
	maxLife float64           // decaying max of observed request lifetimes, in seconds
}

func (w *windowDeque) add(start, end time.Time, tokens int) {
	if tokens <= 0 {
		return
	}
	if w.first.IsZero() || start.Before(w.first) {
		w.first = start
	}
	if w.buckets == nil {
		w.buckets = make(map[int64]float64)
	}
	if !end.After(start) {
		// Degenerate (zero-duration) request: credit the end second.
		w.buckets[end.Unix()] += float64(tokens)
		return
	}
	// Track a decaying max lifetime. Decay slowly (~0.97 per add) so a
	// transient long request keeps the lag elevated for ~30 subsequent
	// completions, preventing oscillation between under-counted and
	// fully-filled regimes.
	life := end.Sub(start).Seconds()
	w.maxLife *= 0.97
	if life > w.maxLife {
		w.maxLife = life
	}
	perSec := float64(tokens) / life
	cur := start
	for cur.Before(end) {
		secStart := time.Unix(cur.Unix(), 0)
		secEnd := secStart.Add(time.Second)
		if secEnd.After(end) {
			secEnd = end
		}
		overlap := secEnd.Sub(cur).Seconds()
		w.buckets[cur.Unix()] += perSec * overlap
		cur = secEnd
	}
}

func (w *windowDeque) tps(now time.Time) (float64, bool) {
	if w.first.IsZero() {
		return 0, false
	}
	// Lag the read by the decaying max of observed request lifetimes
	// so every bucket inside the read window has been fully filled by
	// every request that overlapped it. Mean lag is too short: a
	// bucket at second t is only complete once every request that was
	// alive during t has ended, i.e. t + maxLife.
	lagSec := w.maxLife
	if maxLag := w.maxAge.Seconds() - 1; lagSec > maxLag {
		lagSec = maxLag
	}
	if lagSec < 0 {
		lagSec = 0
	}

	// Use sub-second resolution for the read window so adjacent
	// samples don't return identical sums. The 1-second bucket grid
	// is unchanged; we just weight the trailing edges of the read
	// window by their fractional overlap with each bucket. Tokens
	// inside a bucket are assumed to have been generated uniformly
	// within that second (consistent with how add() distributes them).
	nowF := float64(now.UnixNano()) / 1e9
	readEnd := nowF - lagSec
	readStart := readEnd - w.maxAge.Seconds()

	// Evict buckets older than the absolute retention horizon (well
	// before the cutoff) so the map can't grow unbounded.
	absoluteCutoff := int64(nowF - w.maxAge.Seconds() - maxLagSeconds)
	for sec := range w.buckets {
		if sec < absoluteCutoff {
			delete(w.buckets, sec)
		}
	}

	var sum float64
	startSec := int64(math.Floor(readStart))
	endSec := int64(math.Floor(readEnd))
	for sec, v := range w.buckets {
		if sec < startSec || sec > endSec {
			continue
		}
		l := float64(sec)
		r := l + 1
		if l < readStart {
			l = readStart
		}
		if r > readEnd {
			r = readEnd
		}
		overlap := r - l
		if overlap <= 0 {
			continue
		}
		sum += v * overlap
	}

	covered := w.maxAge.Seconds()
	firstF := float64(w.first.UnixNano()) / 1e9
	if elapsed := readEnd - firstF; elapsed < covered {
		covered = elapsed
	}
	if covered <= 0 {
		return 0, false
	}

	// Ready once the wall-clock has accumulated a full window plus
	// the current lag — at that point every bucket in the read
	// window has been fully filled by every overlapping request.
	ready := (nowF - firstF) >= w.maxAge.Seconds()+lagSec
	return sum / covered, ready
}

// maxLagSeconds bounds how much extra bucket history we retain beyond
// the maxAge window so that the lagged-read can still see them.
const maxLagSeconds = 60.0

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

