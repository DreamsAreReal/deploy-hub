package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/getsentry/sentry-go"
)

const (
	Name           = "starter"
	HealthPath     = "/healthz"
	healthRoute    = "GET " + HealthPath
	homeRoute      = "GET /{$}"
	metricsRoute   = "GET /metrics"
	unmatched      = "unmatched"
	healthBody     = `{"status":"ok"}` + "\n"
	homeBody       = `{"service":"` + Name + `"}` + "\n"
	requestsMetric = `http_requests_total{route=%q,code="%d"}`
	durationMetric = `http_request_duration_seconds{route=%q}`
	flushWait      = 2 * time.Second
	sameOrigin     = "same-origin"
	contentType    = "Content-Type"
	jsonType       = "application/json"
)

var securityHeaders = map[string]string{
	"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
	"X-Frame-Options":              "DENY",
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
	"Cross-Origin-Opener-Policy":   sameOrigin,
	"Cross-Origin-Embedder-Policy": "require-corp",
	"Cross-Origin-Resource-Policy": sameOrigin,
	"Cache-Control":                "no-store",
}

type Service struct {
	log     *slog.Logger
	hub     *sentry.Hub
	metrics *metrics.Set
}

func NewHub(dsn string) (*sentry.Hub, error) {
	client, err := sentry.NewClient(sentry.ClientOptions{Dsn: dsn})
	if err != nil {
		return nil, fmt.Errorf("sentry: %w", err)
	}
	return sentry.NewHub(client, sentry.NewScope()), nil
}

func New(log *slog.Logger, hub *sentry.Hub) *Service {
	return &Service{log: log, hub: hub, metrics: metrics.NewSet()}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthRoute, answer(healthBody))
	mux.HandleFunc(homeRoute, answer(homeBody))
	return s.observe(mux)
}

func (s *Service) Metrics() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(metricsRoute, func(w http.ResponseWriter, _ *http.Request) {
		s.metrics.WritePrometheus(w)
		metrics.WriteProcessMetrics(w)
	})
	return mux
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentType, jsonType)
		_, _ = io.WriteString(w, body)
	}
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Service) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		for name, value := range securityHeaders {
			w.Header().Set(name, value)
		}
		seen := &recorder{ResponseWriter: w, status: http.StatusOK}
		defer s.finish(seen, r, started)
		next.ServeHTTP(seen, r)
	})
}

func (s *Service) finish(w *recorder, r *http.Request, started time.Time) {
	if recovered := recover(); recovered != nil {
		s.hub.RecoverWithContext(r.Context(), recovered)
		s.hub.Flush(flushWait)
		w.WriteHeader(http.StatusInternalServerError)
	}
	route := r.Pattern
	if route == "" {
		route = unmatched
	}
	s.metrics.GetOrCreateCounter(fmt.Sprintf(requestsMetric, route, w.status)).Inc()
	s.metrics.GetOrCreateHistogram(fmt.Sprintf(durationMetric, route)).UpdateDuration(started)
	s.log.Info("request", "method", r.Method, "route", route, "status", w.status, "ms", time.Since(started).Milliseconds())
}
