package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

// LogRequests writes one concise access-log entry after each request.
func LogRequests(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		logged := &loggingResponseWriter{ResponseWriter: w}
		defer func() {
			if panicValue := recover(); panicValue != nil {
				logged.status = http.StatusInternalServerError
				writeRequestLog(logger, r, logged, started)
				panic(panicValue)
			}
			writeRequestLog(logger, r, logged, started)
		}()

		next.ServeHTTP(logged, r)
	})
}

func writeRequestLog(logger *slog.Logger, r *http.Request, logged *loggingResponseWriter, started time.Time) {
	status := logged.status
	if status == 0 {
		status = http.StatusOK
	}
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"duration_ms", time.Since(started).Milliseconds(),
		"bytes", logged.bytes,
	}
	if cacheStatus := logged.Header().Get("X-Inazuma-Cache"); cacheStatus != "" {
		attrs = append(attrs, "cache", cacheStatus)
	}

	if status >= http.StatusInternalServerError {
		logger.Error("request", attrs...)
		return
	}
	logger.Info("request", attrs...)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	// Informational responses do not determine the final response status.
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach optional interfaces implemented by
// the underlying writer (Flusher, Hijacker, and deadline setters).
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
