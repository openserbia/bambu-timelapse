package service

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// reportStaleAfter is how long the telemetry may go quiet before the service
// calls itself unhealthy. The printer reports continuously, a full snapshot
// every 20-55s, so silence rather than a crash is the failure worth catching:
// a process that is up but deaf captures nothing and looks perfectly fine.
const (
	reportStaleAfter  = 5 * time.Minute
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Serve runs the metrics and health listener until ctx is cancelled.
func (s *Service) Serve(ctx context.Context) error {
	r := chi.NewRouter()
	r.Method(http.MethodGet, "/metrics",
		promhttp.HandlerFor(s.m.Registry(), promhttp.HandlerOpts{}))

	health := func(w http.ResponseWriter, _ *http.Request) {
		last := s.lastReport.Load()
		age := time.Duration(0)
		if last > 0 {
			age = time.Since(time.Unix(last, 0))
		}
		// Before the first report there is nothing to be stale about; the
		// container's start-period covers that window.
		ok := s.mqttUp.Load() && (last == 0 || age < reportStaleAfter)

		body := map[string]any{"mqtt": s.mqttUp.Load()}
		if last > 0 {
			body["last_report_age_seconds"] = int(age.Seconds())
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	// GET and HEAD both: some probes HEAD, and chi matches per method.
	for _, path := range []string{"/healthz", "/livez"} {
		r.Get(path, health)
		r.Head(path, health)
	}

	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() {
		<-ctx.Done()
		// WithoutCancel, not Background: this inherits nothing cancellable
		// (ctx is already done here) but keeps any values on it.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	s.log.Info("http listening", "addr", s.cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
