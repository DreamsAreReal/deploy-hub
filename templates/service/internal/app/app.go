package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/DreamsAreReal/starter/internal/web"
)

const (
	healthURL     = "http://127.0.0.1:%s" + web.HealthPath
	shutdownWait  = 5 * time.Second
	probeTimeout  = 3 * time.Second
	headerTimeout = 10 * time.Second
	exitOK        = 0
	exitFailed    = 1
	envPort       = "PORT"
	envMetrics    = "METRICS_PORT"
	envSentryDSN  = "SENTRY_DSN"
	defaultPort   = "8080"
	defaultMetric = "9100"
)

var errUnhealthy = errors.New("health check failed")

type Config struct {
	Port        string
	MetricsPort string
	SentryDSN   string
}

func Load(getenv func(string) string) Config {
	return Config{Port: or(getenv(envPort), defaultPort), MetricsPort: or(getenv(envMetrics), defaultMetric), SentryDSN: getenv(envSentryDSN)}
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func Main(ctx context.Context, args []string, getenv func(string) string, out io.Writer) int {
	flags := flag.NewFlagSet(web.Name, flag.ContinueOnError)
	flags.SetOutput(out)
	healthcheck := flags.Bool("healthcheck", false, "probe the local health endpoint and exit")
	if flags.Parse(args) != nil {
		return exitFailed
	}
	cfg := Load(getenv)
	if *healthcheck {
		return exitCode(probe(ctx, fmt.Sprintf(healthURL, cfg.Port)))
	}
	log := slog.New(slog.NewJSONHandler(out, nil))
	if err := Run(ctx, cfg, log); err != nil {
		log.Error("stopped", "err", err)
		return exitFailed
	}
	return exitOK
}

func exitCode(err error) int {
	if err != nil {
		return exitFailed
	}
	return exitOK
}

func probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", errUnhealthy, response.Status)
	}
	return nil
}

func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	hub, err := web.NewHub(cfg.SentryDSN)
	if err != nil {
		return err
	}
	listeners, err := listen(ctx, cfg.Port, cfg.MetricsPort)
	if err != nil {
		return err
	}
	return Serve(ctx, log, web.New(log, hub), listeners[0], listeners[1])
}

func listen(ctx context.Context, ports ...string) ([]net.Listener, error) {
	var config net.ListenConfig
	listeners := make([]net.Listener, 0, len(ports))
	for _, port := range ports {
		listener, err := config.Listen(ctx, "tcp", ":"+port)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func Serve(ctx context.Context, log *slog.Logger, service *web.Service, site, metrics net.Listener) error {
	servers := map[net.Listener]*http.Server{site: newServer(service.Handler()), metrics: newServer(service.Metrics())}
	stopped := make(chan error, len(servers))
	for listener, server := range servers {
		go func() { stopped <- server.Serve(listener) }()
	}
	log.Info("started", "addr", site.Addr().String(), "metrics", metrics.Addr().String())
	var err error
	select {
	case <-ctx.Done():
	case err = <-stopped:
	}
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
	defer cancel()
	for _, server := range servers {
		err = errors.Join(err, server.Shutdown(grace))
	}
	return err
}

func newServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: headerTimeout}
}
