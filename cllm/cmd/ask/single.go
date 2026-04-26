package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// resolveSinglePrompt returns the canonical user prompt for single-shot
// mode (and as the fallback prompt for bench mode that does not use
// --file). Resolution order:
//   1. Positional args (joined by spaces)
//   2. --prompt
//   3. --prompt-file
//   4. stdin (if not a TTY)
//   5. interactive multi-line read (terminated by a blank line)
//
// In bench mode with --file, the canonical prompt is unused; we return
// "" without prompting interactively.
func resolveSinglePrompt(opts options, stdin io.Reader, stdout, stderr io.Writer) (string, error) {
	if opts.bench > 0 && len(opts.files) > 0 {
		return "", nil
	}

	if len(opts.prompts) > 0 {
		return strings.Join(opts.prompts, " "), nil
	}
	if opts.promptText != "" {
		return opts.promptText, nil
	}
	if opts.promptFile != "" {
		buf, err := os.ReadFile(opts.promptFile)
		if err != nil {
			return "", fmt.Errorf("--prompt-file: %w", err)
		}
		return strings.TrimRight(string(buf), "\n"), nil
	}

	// stdin?
	if f, ok := stdin.(*os.File); ok {
		fi, err := f.Stat()
		if err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
			buf, err := io.ReadAll(f)
			if err != nil {
				return "", err
			}
			text := strings.TrimRight(string(buf), "\n")
			if text == "" {
				return "", errors.New("empty prompt on stdin")
			}
			return text, nil
		}
	}

	// Interactive multi-line prompt; terminate with a blank line.
	fmt.Fprintln(stderr, "User prompt (terminate with a blank line):")
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var b strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "", errors.New("empty prompt")
	}
	return b.String(), nil
}

// runSingle handles the one-shot path: stream the assistant content live
// to stdout, then print a trailer summary.
func runSingle(opts options, prompt string, stdout, stderr io.Writer) error {
	ctx := context.Background()
	httpClient := &http.Client{Transport: http.DefaultTransport}

	var liveOut io.Writer
	if !opts.quiet && !opts.json && opts.stream {
		liveOut = stdout
	}
	var debugOut io.Writer
	if opts.debug {
		debugOut = stderr
	}

	r := sendRequest(ctx, httpClient, opts, prompt, opts.dsl, 0, -1, liveOut, debugOut)

	if opts.json {
		return writeResultJSON(stdout, r)
	}
	if !opts.stream && !opts.quiet {
		// Non-streaming: we never wrote content live; print it now.
		fmt.Fprint(stdout, r.Body)
	}
	if !opts.quiet && opts.stream {
		fmt.Fprintln(stdout)
	}

	printTrailer(stdout, r)
	if !r.OK {
		return fmt.Errorf("request failed: %s", r.Err)
	}
	return nil
}

// printTrailer renders the human-readable summary line block that the old
// ask.sh emitted, augmented with ttft and decode tok/s.
func printTrailer(w io.Writer, r Result) {
	fmt.Fprintln(w, "------------------")
	fmt.Fprintf(w, "elapsed_ms: %d\n", r.Duration().Milliseconds())
	if r.TTFT >= 0 {
		fmt.Fprintf(w, "ttft_ms: %d\n", r.TTFT.Milliseconds())
	} else {
		fmt.Fprintln(w, "ttft_ms: n/a")
	}
	fmt.Fprintf(w, "cache: %t\n", r.CacheHit)
	if !r.OK {
		fmt.Fprintf(w, "error: %s\n", r.Err)
		return
	}
	fmt.Fprintf(w, "prompt_tokens: %d\n", r.PromptTokens)
	fmt.Fprintf(w, "completion_tokens: %d\n", r.CompletionTokens)
	fmt.Fprintf(w, "total_tokens: %d\n", r.TotalTokens)
	if tps := r.DecodeTPS(); tps > 0 {
		fmt.Fprintf(w, "tokens_per_second: %.2f\n", tps)
	} else {
		fmt.Fprintln(w, "tokens_per_second: n/a")
	}
}

// writeResultJSON emits a Result as a single JSON line.
func writeResultJSON(w io.Writer, r Result) error {
	type wire struct {
		OK               bool    `json:"ok"`
		Worker           int     `json:"worker"`
		PromptIndex      int     `json:"prompt_index"`
		Prompt           string  `json:"prompt,omitempty"`
		StatusCode       int     `json:"status_code"`
		StartedAt        string  `json:"started_at"`
		EndedAt          string  `json:"ended_at"`
		ElapsedMs        int64   `json:"elapsed_ms"`
		TTFTMs           int64   `json:"ttft_ms"`
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
		DecodeTPS        float64 `json:"decode_tokens_per_second"`
		CacheHit         bool    `json:"cache_hit"`
		Err              string  `json:"error,omitempty"`
	}
	row := wire{
		OK:               r.OK,
		Worker:           r.Worker,
		PromptIndex:      r.PromptIndex,
		Prompt:           r.Prompt,
		StatusCode:       r.StatusCode,
		StartedAt:        r.StartedAt.Format("2006-01-02T15:04:05.000Z07:00"),
		EndedAt:          r.EndedAt.Format("2006-01-02T15:04:05.000Z07:00"),
		ElapsedMs:        r.Duration().Milliseconds(),
		TTFTMs:           -1,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		DecodeTPS:        r.DecodeTPS(),
		CacheHit:         r.CacheHit,
		Err:              r.Err,
	}
	if r.TTFT >= 0 {
		row.TTFTMs = r.TTFT.Milliseconds()
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(row)
}
