package httpapi

import (
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

type Handler struct {
	ready atomic.Bool
}

func NewHandler() *Handler {
	handler := &Handler{}
	handler.ready.Store(true)
	return handler
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /ask", h.ask)
	return requestLogger(mux)
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writePlainText(w, http.StatusOK, "ok\n")
}

func (h *Handler) readyz(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		writePlainText(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}

	writePlainText(w, http.StatusOK, "ready\n")
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writePlainText(w, http.StatusBadRequest, "missing q\n")
		return
	}

	writePlainText(w, http.StatusOK, query+"\n")
}

func writePlainText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	bytes      int
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	bytesWritten, err := w.ResponseWriter.Write(body)
	w.bytes += bytesWritten
	return bytesWritten, err
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		responseWriter := &loggingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(responseWriter, r)

		statusCode := responseWriter.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}

		log.Printf(
			"method=%s path=%s status=%d bytes=%d duration_ms=%.2f",
			r.Method,
			r.URL.RequestURI(),
			statusCode,
			responseWriter.bytes,
			float64(time.Since(startedAt))/float64(time.Millisecond),
		)
	})
}
