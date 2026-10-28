package server

import (
	"net/http"
	"time"
)

// RequestLogger receives one entry per finished request.
type RequestLogger func(at time.Time, method, path string, status int, took time.Duration)

// statusRecorder captures the status a handler sent. Unwrap lets
// http.ResponseController reach the real writer, so SSE flushing still works
// through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// AccessLog wraps next and reports every request to log once it finishes. It
// sits outermost, so rejections by the origin guard and auth are logged too.
func AccessLog(next http.Handler, log RequestLogger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log(start, r.Method, r.URL.RequestURI(), status, time.Since(start))
	})
}
