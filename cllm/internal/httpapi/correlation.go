package httpapi

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

const (
	requestIDHeader        = "X-Request-ID"
	requestIDLogKey        = "request_id"
	requestIDMaxLen        = 128
	chatCompletionEndpoint = "chat_completions"
)

type contextKey string

const requestIDContextKey contextKey = "request_id"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validateRequestID returns the input if it satisfies the accepted format, else "".
func validateRequestID(value string) string {
	if value == "" || len(value) > requestIDMaxLen {
		return ""
	}
	if !requestIDPattern.MatchString(value) {
		return ""
	}
	return value
}

// withRequestID returns a context carrying the given request ID.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey, id)
}

// requestIDFromContext returns the request ID associated with ctx, or "".
func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDContextKey).(string); ok {
		return id
	}
	return ""
}

// loggerFromContext returns slog.Default() augmented with the request_id attr if present.
func loggerFromContext(ctx context.Context) *slog.Logger {
	id := requestIDFromContext(ctx)
	if id == "" {
		return slog.Default()
	}
	return slog.Default().With(requestIDLogKey, id)
}

// newULID generates a 26-char Crockford base32 ULID (48-bit ms timestamp + 80-bit random).
func newULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	_, _ = rand.Read(b[6:])
	return encodeULID(b)
}

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func encodeULID(b [16]byte) string {
	out := make([]byte, 26)
	// First two chars encode b[0] (8 bits) into 5+5; top 2 bits of first char are 0.
	out[0] = crockfordAlphabet[(b[0]>>5)&0x1f]
	out[1] = crockfordAlphabet[b[0]&0x1f]
	// Pack remaining 15 bytes (120 bits) into 24 chars (5 bits each).
	var bits uint64
	var nbits uint
	pos := 2
	for i := 1; i < 16; i++ {
		bits = (bits << 8) | uint64(b[i])
		nbits += 8
		for nbits >= 5 {
			nbits -= 5
			out[pos] = crockfordAlphabet[(bits>>nbits)&0x1f]
			pos++
		}
	}
	return string(out)
}

// resolveRequestID returns a validated inbound id or a freshly generated ULID.
func resolveRequestID(r *http.Request) string {
	if id := validateRequestID(r.Header.Get(requestIDHeader)); id != "" {
		return id
	}
	return newULID()
}

// emitLifecycleEvent records a lifecycle event log line at the given level and increments
// the matching Prometheus counter. The "event" attr is always added; "outcome" is added
// when non-empty so callers do not have to repeat the bookkeeping at each call site.
func (h *Handler) emitLifecycleEvent(ctx context.Context, level slog.Level, event, outcome, msg string, attrs ...any) {
	h.metrics.observeLifecycleEvent(chatCompletionEndpoint, event, outcome)
	logger := loggerFromContext(ctx)
	base := make([]any, 0, len(attrs)+4)
	base = append(base, "event", event)
	if outcome != "" {
		base = append(base, "outcome", outcome)
	}
	base = append(base, attrs...)
	logger.Log(ctx, level, msg, base...)
}
