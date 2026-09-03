package httpx

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogRequestsWritesOneLineWithResponseDetails(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{ReplaceAttr: omitLogTime}))
	handler := LogRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Inazuma-Cache", "HIT")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/wiki/Pikachu?ignored=yes", nil))

	got := strings.TrimSpace(output.String())
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("log contains more than one line: %q", got)
	}
	for _, want := range []string{"level=INFO", "msg=request", "method=GET", "path=/wiki/Pikachu", "status=201", "bytes=5", "cache=HIT"} {
		if !strings.Contains(got, want) {
			t.Errorf("log = %q, want field %q", got, want)
		}
	}
	if strings.Contains(got, "ignored=yes") {
		t.Fatalf("log includes query string: %q", got)
	}
}

func TestLogRequestsUsesErrorLevelForServerError(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{ReplaceAttr: omitLogTime}))
	handler := LogRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/wiki/Pikachu", nil))

	if got := output.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "status=503") {
		t.Fatalf("log = %q, want ERROR with status 503", got)
	}
}

func TestLogRequestsLogsPanicsAsServerErrors(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{ReplaceAttr: omitLogTime}))
	handler := LogRequests(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()

	if got := output.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "status=500") {
		t.Fatalf("log = %q, want ERROR with status 500", got)
	}
}

func omitLogTime(_ []string, attr slog.Attr) slog.Attr {
	if attr.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return attr
}
