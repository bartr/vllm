// Command ask is a single CLI for talking to an OpenAI-compatible chat
// completions endpoint, either as a one-shot interactive request or as a
// concurrent benchmark. It replaces the previous bash scripts ask.sh and
// benchmark.sh.
//
// Usage:
//
//	ask [OPTIONS] [PROMPT...]
//
// See `ask --help` for the full flag list.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// options collects every flag and resolved environment value that drives a
// run. It is filled by parseFlags and then passed by value to the runners.
type options struct {
	// Mode and stop conditions.
	bench        int           // 0 = single-shot
	rampStart    int           // start of linear ramp; 0 means no ramp
	rampEnd      int
	rampDuration time.Duration
	duration     time.Duration // 0 = no time limit
	count        int           // 0 = no count limit
	loop         int           // 0 = no loop limit (file mode only)
	random       bool

	// Endpoint.
	url       string
	token     string
	model     string
	waitReady bool

	// Request shape.
	systemPrompt string
	maxTokens    int
	temperature  float64
	stream       bool
	dsl          string

	// Prompt sourcing.
	prompts     []string  // positional args
	promptText  string    // --prompt
	promptFile  string    // --prompt-file
	files       []string  // --file (repeatable)

	// Output.
	quiet  bool
	json   bool
	debug  bool
	report bool

	// Bench-only behavior.
	warmup bool
}

func main() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	// Resolve the user prompt for single-shot mode (and as the canonical
	// prompt when bench mode runs without --file).
	prompt, err := resolveSinglePrompt(opts, stdin, stdout, stderr)
	if err != nil {
		return err
	}

	if opts.waitReady {
		if err := waitForHealth(opts); err != nil {
			return err
		}
	}

	if opts.model == "" {
		m, err := autodetectModel(opts)
		if err != nil {
			return err
		}
		opts.model = m
	}

	if opts.bench > 0 {
		return runBench(opts, prompt, stdout, stderr)
	}
	return runSingle(opts, prompt, stdout, stderr)
}

// parseFlags is broken out so test code can construct an options without
// invoking the global flag set.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	opts := defaultOptions()

	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)

	// Mode.
	fs.IntVar(&opts.bench, "bench", 0, "run N concurrent workers (bench mode)")

	// Stop conditions.
	var rampSpec string
	fs.StringVar(&rampSpec, "ramp", "", "linear ramp `START:END` of concurrent workers (bench mode)")
	fs.DurationVar(&opts.rampDuration, "ramp-duration", 30*time.Second, "duration of the ramp; 0 = instant jump")
	var durationSpec string
	fs.StringVar(&durationSpec, "duration", "", "stop bench after duration (e.g. 30s, 5m, 1h)")
	fs.IntVar(&opts.count, "count", 0, "stop bench after N completed requests")
	fs.IntVar(&opts.loop, "loop", 0, "loop the prompt list N times (requires --file)")
	fs.BoolVar(&opts.random, "random", false, "pick a random prompt per request (requires --file)")

	// Endpoint.
	fs.StringVar(&opts.url, "url", opts.url, "base URL of the chat endpoint")
	fs.StringVar(&opts.token, "token", opts.token, "bearer token for authenticated endpoints")
	fs.StringVar(&opts.model, "model", opts.model, "model id (auto-detected via /v1/models if omitted)")
	fs.BoolVar(&opts.waitReady, "wait-ready", false, "poll /health until the endpoint is ready before starting")

	// Request shape.
	fs.StringVar(&opts.systemPrompt, "system", opts.systemPrompt, "system prompt")
	fs.IntVar(&opts.maxTokens, "max-tokens", opts.maxTokens, "max completion tokens per request")
	fs.Float64Var(&opts.temperature, "temperature", opts.temperature, "sampling temperature")
	noStream := false
	fs.BoolVar(&opts.stream, "stream", true, "use streaming responses (default true)")
	fs.BoolVar(&noStream, "no-stream", false, "disable streaming (overrides --stream)")
	fs.StringVar(&opts.dsl, "dsl", opts.dsl, "DSL directives prepended to the prompt as ':dsl TEXT\\n'")

	// Prompt sourcing.
	fs.StringVar(&opts.promptText, "prompt", opts.promptText, "user prompt text (alternative to positional args)")
	fs.StringVar(&opts.promptFile, "prompt-file", opts.promptFile, "read user prompt from a text file")
	fs.Func("file", "YAML file of prompts (bench mode only; repeatable)", func(v string) error {
		opts.files = append(opts.files, v)
		return nil
	})

	// Output.
	fs.BoolVar(&opts.quiet, "quiet", false, "suppress streamed content (single mode shows only the trailer)")
	fs.BoolVar(&opts.quiet, "q", false, "alias for --quiet")
	fs.BoolVar(&opts.json, "json", false, "emit one JSON Result per request (bench: NDJSON; disables tail/report)")
	fs.BoolVar(&opts.debug, "debug", false, "print raw SSE lines to stderr while streaming")
	noReport := false
	fs.BoolVar(&opts.report, "report", true, "print summary report at end of bench (default true)")
	fs.BoolVar(&noReport, "no-report", false, "disable summary report")

	// Bench-only.
	fs.BoolVar(&opts.warmup, "warmup", false, "run one untimed warmup request before bench workers start")

	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
	}

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	if noStream {
		opts.stream = false
	}
	if noReport {
		opts.report = false
	}
	opts.prompts = fs.Args()

	if rampSpec != "" {
		s, e, err := parseRampSpec(rampSpec)
		if err != nil {
			return options{}, err
		}
		opts.rampStart, opts.rampEnd = s, e
	}
	if durationSpec != "" {
		d, err := parseDurationSpec(durationSpec)
		if err != nil {
			return options{}, err
		}
		opts.duration = d
	}

	if err := validateOptions(opts); err != nil {
		return options{}, err
	}
	return opts, nil
}

func defaultOptions() options {
	return options{
		url:          envOr("CLLM_URL", "http://localhost:8088"),
		token:        envOr("CLLM_TOKEN", ""),
		model:        envOr("CLLM_MODEL", ""),
		systemPrompt: envOr("CLLM_SYSTEM_PROMPT", "You are a helpful assistant."),
		maxTokens:    envIntOr("CLLM_MAX_TOKENS", 1024),
		temperature:  envFloatOr("CLLM_TEMPERATURE", 0.2),
		stream:       true,
		dsl:          envOr("CLLM_DSL", ""),
		report:       true,
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloatOr(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func parseRampSpec(s string) (int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid --ramp %q (expected START:END)", s)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 1 {
		return 0, 0, fmt.Errorf("invalid --ramp start %q", parts[0])
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil || end < start {
		return 0, 0, fmt.Errorf("invalid --ramp end %q (must be >= start)", parts[1])
	}
	return start, end, nil
}

// parseDurationSpec accepts both Go-native durations (10s, 1m30s) and the
// shorthand the old benchmark.sh used (10s, 5m, 1h, or a bare integer
// number of seconds).
func parseDurationSpec(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

func validateOptions(opts options) error {
	if opts.bench < 0 {
		return errors.New("--bench must be >= 0")
	}
	if opts.bench == 0 {
		// Single-shot: bench-only flags should not be set.
		if opts.duration > 0 || opts.count > 0 || opts.loop > 0 ||
			opts.random || opts.warmup || len(opts.files) > 0 ||
			opts.rampEnd > 0 {
			return errors.New("bench-only flags require --bench N")
		}
		return nil
	}
	if opts.rampEnd > 0 {
		if opts.rampEnd > opts.bench {
			return fmt.Errorf("--ramp end (%d) cannot exceed --bench (%d)", opts.rampEnd, opts.bench)
		}
	}
	if opts.loop > 0 && len(opts.files) == 0 {
		return errors.New("--loop requires --file")
	}
	if opts.random && len(opts.files) == 0 {
		return errors.New("--random requires --file")
	}
	if opts.maxTokens < 1 {
		return errors.New("--max-tokens must be >= 1")
	}
	return nil
}

const usageText = `Usage: ask [OPTIONS] [PROMPT...]

Talk to an OpenAI-compatible chat endpoint, either one-shot (default) or as a
concurrent benchmark (--bench N).

Single-shot:
  ask explain azure
  ask --dsl 'profile=fast' explain azure
  ask --no-stream --max-tokens 50 hello
  echo "summarize this" | ask
  ask                              # interactive (terminate prompt with blank line)

Bench:
  ask --bench 20 --duration 30s --prompt 'explain azure'
  ask --bench 50 --ramp 1:50 --ramp-duration 30s --duration 2m --prompt 'hi'
  ask --bench 8 --file prompts.yaml          # process each prompt once, then exit
  ask --bench 8 --file prompts.yaml --loop 5 # process the list 5 times
  ask --bench 8 --file prompts.yaml --random --duration 1m

YAML prompt file format:
  - prompt: "Explain Azure"
    dsl: "profile=fast"          # optional
  - prompt: "What is Kubernetes?"

Mode:
  --bench N                Run N concurrent workers (default: 0 = single-shot)

Stop conditions (bench, first-to-fire wins; Ctrl-C always works):
  --duration DUR           Stop after wall-clock duration (e.g. 30s, 5m, 1h)
  --count N                Stop after N completed requests
  --loop N                 Iterate --file prompt list N times (requires --file)

Concurrency shape (bench):
  --ramp START:END         Linear ramp from START to END concurrent workers
  --ramp-duration DUR      Duration of the ramp (default 30s); 0 = instant jump
  --warmup                 Run one untimed warmup request before workers start

Prompt sourcing:
  PROMPT...                Positional args become the user prompt
  --prompt TEXT            Alternative to positional args
  --prompt-file PATH       Read prompt from text file
  --file PATH              YAML file of prompts (bench-only; repeatable)
  --random                 Pick a random prompt per request (requires --file)

Request shape:
  --url URL                Endpoint base URL (default $CLLM_URL or http://localhost:8088)
  --token TOKEN            Bearer token (default $CLLM_TOKEN)
  --model MODEL            Model id (default $CLLM_MODEL; auto-detected if omitted)
  --system PROMPT          System prompt (default $CLLM_SYSTEM_PROMPT)
  --max-tokens N           Max completion tokens per request (default 1024)
  --temperature F          Sampling temperature (default 0.2)
  --stream / --no-stream   Streaming on/off (default on)
  --dsl 'TEXT'             Prepend ':dsl TEXT\n' to every user prompt
  --wait-ready             Poll /health until ready before starting

Output:
  -q, --quiet              Suppress streamed content; show only the trailer
  --json                   Emit one JSON Result per request (NDJSON in bench)
  --debug                  Print SSE lines to stderr while streaming
  --report / --no-report   Final summary in bench mode (default on)
  -h, --help               Show this help

Environment (lower precedence than flags):
  CLLM_URL, CLLM_TOKEN, CLLM_MODEL, CLLM_SYSTEM_PROMPT,
  CLLM_MAX_TOKENS, CLLM_TEMPERATURE, CLLM_DSL
`
