package httpapi

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	t.Parallel()

	handler := NewHandler().Routes()

	tests := []struct {
		name       string
		method     string
		path       string
		statusCode int
		body       string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz", statusCode: http.StatusOK, body: "ok\n"},
		{name: "readyz", method: http.MethodGet, path: "/readyz", statusCode: http.StatusOK, body: "ready\n"},
		{name: "ask", method: http.MethodGet, path: "/ask", statusCode: http.StatusOK, body: "success\n"},
		{name: "ask method not allowed", method: http.MethodPost, path: "/ask", statusCode: http.StatusMethodNotAllowed, body: "Method Not Allowed\n"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			if recorder.Code != test.statusCode {
				t.Fatalf("status code = %d, want %d", recorder.Code, test.statusCode)
			}

			if recorder.Body.String() != test.body {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.body)
			}
		})
	}
}

func TestRoutesRequestLogging(t *testing.T) {
	t.Parallel()

	var logBuffer bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logBuffer)
	defer log.SetOutput(originalOutput)

	handler := NewHandler().Routes()
	req := httptest.NewRequest(http.MethodGet, "/ask", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	logLine := logBuffer.String()
	for _, want := range []string{
		"method=GET",
		"path=/ask",
		"status=200",
		"bytes=8",
	} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log line %q does not contain %q", logLine, want)
		}
	}

	matched, err := regexp.MatchString(`duration_ms=\d+\.\d{2}`, logLine)
	if err != nil {
		t.Fatalf("match duration regex: %v", err)
	}
	if !matched {
		t.Fatalf("log line %q does not contain duration_ms with two decimals", logLine)
	}
}
