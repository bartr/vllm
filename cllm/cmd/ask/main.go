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

	"cllm/internal/buildinfo"
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
	loop         bool          // cycle prompt list forever (file mode)
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
	if hasVersionFlag(args) {
		_, _ = fmt.Fprintf(stdout, "ask %s\n", buildinfo.Version)
		return nil
	}

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

	// Pre-scan: --files takes one-or-more non-flag tokens until the next
	// `-`-prefixed argument or end of args. Splice them out before
	// handing the remainder to the standard flag package, which only
	// supports single-value flags.
	args, files := extractFilesArg(args)
	opts.files = files

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
	fs.BoolVar(&opts.loop, "loop", false, "cycle the prompt list forever (requires --files; use --count/--duration to bound)")
	fs.BoolVar(&opts.random, "random", false, "pick a random prompt per request (requires --files)")

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

func hasVersionFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--version" {
			return true
		}
	}
	return false
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
		if opts.duration > 0 || opts.count > 0 || opts.loop ||
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
	if opts.loop && len(opts.files) == 0 {
		return errors.New("--loop requires --files")
	}
	if opts.random && len(opts.files) == 0 {
		return errors.New("--random requires --files")
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
  ask --bench 8 --files prompts.yaml             # process each prompt once, then exit
  ask --bench 8 --files prompts.yaml --loop      # cycle forever (Ctrl-C to stop)
  ask --bench 8 --files prompts.yaml --count 1000           # 1000 reqs cycling list
  ask --bench 8 --files a.yaml b.yaml c.yaml --random --duration 1m

YAML prompt file format:
  - prompt: "Explain Azure"
    dsl: "profile=fast"          # optional
  - prompt: "What is Kubernetes?"

Mode:
  --bench N                Run N concurrent workers (default: 0 = single-shot)

Stop conditions (bench, first-to-fire wins; Ctrl-C always works):
  --duration DUR           Stop after wall-clock duration (e.g. 30s, 5m, 1h)
  --count N                Stop after N completed requests
  --loop                   Cycle the --files prompt list forever (use --count
                           or --duration to bound). Without --loop, --count or
                           --duration also cycle the list automatically.

Concurrency shape (bench):
  --ramp START:END         Linear ramp from START to END concurrent workers
  --ramp-duration DUR      Duration of the ramp (default 30s); 0 = instant jump
  --warmup                 Run one untimed warmup request before workers start

Prompt sourcing:
  PROMPT...                Positional args become the user prompt
  --prompt TEXT            Alternative to positional args
  --prompt-file PATH       Read prompt from text file
  --files PATH...          One or more YAML prompt files (bench-only).
                           Files are concatenated in order; --files may be
                           repeated, and each occurrence accepts multiple
                           paths until the next flag.
  --random                 Pick a random prompt per request (requires --files)

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
  --version                Print version and exit

Environment (lower precedence than flags):
  CLLM_URL, CLLM_TOKEN, CLLM_MODEL, CLLM_SYSTEM_PROMPT,
  CLLM_MAX_TOKENS, CLLM_TEMPERATURE, CLLM_DSL
`

// extractFilesArg pulls a `--files`/`-files`/`--files=...` flag out of
// args, gathering one-or-more values that follow it (until the next
// `-`-prefixed token or end of args). Returns the remaining args plus
// the collected file list. Multiple `--files` occurrences accumulate.
//
// Recognized forms:
//
//	--files a.yaml b.yaml c.yaml
//	-files a.yaml
//	--files=a.yaml
//	--files=a.yaml b.yaml          (the =value plus following bare args)
func extractFilesArg(args []string) ([]string, []string) {
	var rest []string
	var files []string
	i := 0
	for i < len(args) {
		a := args[i]
		// Stop scanning past `--`: pass it and everything after through.
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, hasValue := splitFlagToken(a)
		if name != "files" {
			rest = append(rest, a)
			i++
			continue
		}
		i++
		if hasValue {
			if value != "" {
				files = append(files, value)
			}
		}
		// Greedily absorb following non-flag tokens.
		for i < len(args) {
			next := args[i]
			if next == "--" || strings.HasPrefix(next, "-") {
				break
			}
			files = append(files, next)
			i++
		}
	}
	return rest, files
}

// splitFlagToken parses "-name", "--name", "--name=value" forms.
// Returns name="" when the token isn't a recognized flag form.
func splitFlagToken(s string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(s, "-") || s == "-" || s == "--" {
		return "", "", false
	}
	t := strings.TrimLeft(s, "-")
	if eq := strings.IndexByte(t, '='); eq >= 0 {
		return t[:eq], t[eq+1:], true
	}
	return t, "", false
}
