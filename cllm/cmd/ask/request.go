package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result captures the wall-clock measurements for a single chat completion
// request. It is written by sendRequest and consumed by the single-shot
// trailer, the bench tail printer, and the final report.
type Result struct {
	OK              bool
	Worker          int
	PromptIndex     int    // index in the prompt source (-1 for the canonical single-shot prompt)
	Prompt          string // original prompt text (without DSL prefix)
	StatusCode      int
	StartedAt       time.Time
	EndedAt         time.Time
	TTFT            time.Duration // -1 if no first token observed
	PromptTokens    int
	CompletionTokens int
	TotalTokens     int
	CacheHit        bool
	Err             string
	// Body, when non-nil, is the streamed assistant content. Single-shot
	// mode prints it live; bench mode discards it.
	Body string
}

// Duration is the total wall-clock latency of the request.
func (r Result) Duration() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// DecodeTPS is the throughput attributable to the streaming/decode phase
// (i.e. excluding queue + prefill). Returns 0 when not measurable.
func (r Result) DecodeTPS() float64 {
	if !r.OK || r.CompletionTokens <= 0 {
		return 0
	}
	decode := r.Duration() - r.TTFT
	if r.TTFT < 0 || decode <= 0 {
		return 0
	}
	return float64(r.CompletionTokens) / decode.Seconds()
}

// chatRequest is the Chat Completions API request body.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Temperature   float64       `json:"temperature"`
	MaxTokens     int           `json:"max_tokens"`
	Stream        bool          `json:"stream,omitempty"`
	StreamOptions *streamOpts   `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// applyDSL prefixes the user prompt with a ":dsl ..." line if --dsl was
// set or if the per-prompt DSL field is non-empty.
func applyDSL(prompt, dsl string) string {
	dsl = strings.TrimSpace(dsl)
	if dsl == "" {
		return prompt
	}
	return ":dsl " + dsl + "\n" + prompt
}

// buildRequest assembles the JSON request body.
func buildRequest(opts options, prompt, dsl string) ([]byte, error) {
	req := chatRequest{
		Model:       opts.model,
		Temperature: opts.temperature,
		MaxTokens:   opts.maxTokens,
		Stream:      opts.stream,
		Messages: []chatMessage{
			{Role: "system", Content: opts.systemPrompt},
			{Role: "user", Content: applyDSL(prompt, dsl)},
		},
	}
	if opts.stream {
		req.StreamOptions = &streamOpts{IncludeUsage: true}
	}
	return json.Marshal(req)
}

// sendRequest performs one chat completion. When liveContent is non-nil
// and streaming is enabled, content deltas are written to it as they
// arrive. The Result is always populated; on error r.OK is false and
// r.Err carries a human-readable message.
//
// httpClient must have no Timeout set when streaming for long requests;
// the caller controls cancellation via ctx.
func sendRequest(ctx context.Context, httpClient *http.Client, opts options, prompt, dsl string, worker, promptIdx int, liveContent io.Writer, debugSSE io.Writer) Result {
	r := Result{
		Worker:      worker,
		PromptIndex: promptIdx,
		Prompt:      prompt,
		TTFT:        -1,
		StartedAt:   time.Now(),
	}

	body, err := buildRequest(opts, prompt, dsl)
	if err != nil {
		r.EndedAt = time.Now()
		r.Err = "build request: " + err.Error()
		return r
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		r.EndedAt = time.Now()
		r.Err = "new request: " + err.Error()
		return r
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Disable transparent gzip on the SSE stream. Go's default transport
	// advertises Accept-Encoding: gzip and decodes inline through a gzip
	// reader, which buffers multiple SSE chunks together and inflates
	// TTFT proportional to the gzip block size. Asking for identity
	// guarantees the server emits the same wire bytes our scanner sees.
	httpReq.Header.Set("Accept-Encoding", "identity")
	if opts.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+opts.token)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		r.EndedAt = time.Now()
		r.Err = "http: " + err.Error()
		return r
	}
	defer resp.Body.Close()

	r.StatusCode = resp.StatusCode
	if v := resp.Header.Get("X-Cache-Hit"); v != "" {
		r.CacheHit = strings.EqualFold(v, "true") || v == "1"
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		r.EndedAt = time.Now()
		r.Err = fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
		return r
	}

	if opts.stream {
		if err := r.consumeSSE(resp.Body, liveContent, debugSSE); err != nil {
			r.EndedAt = time.Now()
			r.Err = "stream: " + err.Error()
			return r
		}
	} else {
		if err := r.consumeJSON(resp.Body); err != nil {
			r.EndedAt = time.Now()
			r.Err = "decode: " + err.Error()
			return r
		}
	}

	r.EndedAt = time.Now()
	r.OK = true
	return r
}

// consumeSSE reads a Server-Sent Events stream of chat completion chunks,
// optionally tee-ing content deltas to liveOut and full lines to debugOut.
// It populates TTFT, token counters, cache flag, and Body.
func (r *Result) consumeSSE(stream io.Reader, liveOut, debugOut io.Writer) error {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var content strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if debugOut != nil {
			fmt.Fprintln(debugOut, line)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[len("data: "):]
		if payload == "[DONE]" {
			break
		}

		var chunk struct {
			Cache   *bool `json:"cache"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate transient parse errors mid-stream
		}
		if chunk.Cache != nil {
			r.CacheHit = *chunk.Cache
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content == "" {
				continue
			}
			if r.TTFT < 0 {
				r.TTFT = time.Since(r.StartedAt)
			}
			content.WriteString(c.Delta.Content)
			if liveOut != nil {
				if _, err := io.WriteString(liveOut, c.Delta.Content); err != nil {
					return err
				}
			}
		}
		if chunk.Usage != nil {
			r.PromptTokens = chunk.Usage.PromptTokens
			r.CompletionTokens = chunk.Usage.CompletionTokens
			r.TotalTokens = chunk.Usage.TotalTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	r.Body = content.String()
	return nil
}

// consumeJSON parses a non-streaming chat completion response.
func (r *Result) consumeJSON(body io.Reader) error {
	var resp struct {
		Cache   *bool `json:"cache"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return err
	}
	if resp.Cache != nil {
		r.CacheHit = *resp.Cache
	}
	if len(resp.Choices) > 0 {
		r.Body = resp.Choices[0].Message.Content
	}
	r.PromptTokens = resp.Usage.PromptTokens
	r.CompletionTokens = resp.Usage.CompletionTokens
	r.TotalTokens = resp.Usage.TotalTokens
	return nil
}

// waitForHealth polls the /health endpoint until it returns 200 or the
// max attempt count is exceeded.
func waitForHealth(opts options) error {
	const attempts = 60
	const delay = 2 * time.Second

	client := &http.Client{Timeout: 2 * time.Second}
	url := opts.url + "/health"
	for i := 1; i <= attempts; i++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if opts.token != "" {
			req.Header.Set("Authorization", "Bearer "+opts.token)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if i == attempts {
			return fmt.Errorf("endpoint did not become ready at %s after %d attempts", url, attempts)
		}
		time.Sleep(delay)
	}
	return errors.New("unreachable")
}

// autodetectModel queries /v1/models and returns the first model id.
func autodetectModel(opts options) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, opts.url+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	if opts.token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /v1/models returned %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", errors.New("no models reported by /v1/models")
	}
	return out.Data[0].ID, nil
}
