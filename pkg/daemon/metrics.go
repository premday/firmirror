package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcomes of a run, and of a check of the rollout rings.
const (
	resultSuccess   = "success"
	resultFailure   = "failure"
	resultPartial   = "partial"
	resultUnchanged = "unchanged"
	resultChanged   = "changed"
	resultError     = "error"
)

// Metrics are what the daemon exports.
type Metrics struct {
	registry *prometheus.Registry

	refreshRuns        *prometheus.CounterVec
	refreshDuration    prometheus.Histogram
	refreshLastSuccess prometheus.Gauge
	refreshNew         prometheus.Gauge
	regenRuns          *prometheus.CounterVec
	regenLastSuccess   prometheus.Gauge
	bkcChecks          *prometheus.CounterVec
	bkcLastCheck       prometheus.Gauge
	bkcLastChange      prometheus.Gauge
	running            *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		refreshRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "firmirror_refresh_runs_total",
			Help: "Refreshes run, by result: success, partial (a vendor or the package cleanup failed, the rings were refused, the run was stopped for its timeout, or what was mirrored could not be published yet) or failure.",
		}, []string{"result"}),
		refreshDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "firmirror_refresh_duration_seconds",
			Help:    "How long a refresh took.",
			Buckets: prometheus.ExponentialBuckets(30, 2, 12),
		}),
		refreshLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "firmirror_refresh_last_success_timestamp_seconds",
			Help: "When a refresh last succeeded, in Unix time. 0 if none has.",
		}),
		refreshNew: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "firmirror_refresh_new_components",
			Help: "Components the last refresh mirrored.",
		}),
		regenRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "firmirror_regen_runs_total",
			Help: "Metadata regenerations after a change of the rollout rings, by result: success, unchanged (nothing to publish) or failure.",
		}, []string{"result"}),
		regenLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "firmirror_regen_last_success_timestamp_seconds",
			Help: "When the metadata was last regenerated after a change of the rollout rings, in Unix time. 0 if never.",
		}),
		bkcChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "firmirror_bkc_checks_total",
			Help: "Reads of the rollout rings, by result: unchanged, changed or error (unreadable or invalid).",
		}, []string{"result"}),
		bkcLastCheck: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "firmirror_bkc_last_check_timestamp_seconds",
			Help: "When the rollout rings were last read successfully, in Unix time. 0 if never.",
		}),
		bkcLastChange: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "firmirror_bkc_last_change_timestamp_seconds",
			Help: "When the rollout rings were last seen to change, in Unix time. 0 if never.",
		}),
		running: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "firmirror_run_in_progress",
			Help: "1 while a run of the kind is going on: refresh or regen.",
		}, []string{"kind"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
		m.refreshRuns, m.refreshDuration, m.refreshLastSuccess, m.refreshNew,
		m.regenRuns, m.regenLastSuccess,
		m.bkcChecks, m.bkcLastCheck, m.bkcLastChange, m.running,
	)
	// A series that is never set is not exported, which alerts cannot tell from a
	// daemon that is not running.
	for _, result := range []string{resultSuccess, resultPartial, resultFailure} {
		m.refreshRuns.WithLabelValues(result)
	}
	for _, result := range []string{resultSuccess, resultUnchanged, resultFailure} {
		m.regenRuns.WithLabelValues(result)
	}
	for _, result := range []string{resultUnchanged, resultChanged, resultError} {
		m.bkcChecks.WithLabelValues(result)
	}
	m.running.WithLabelValues("refresh")
	m.running.WithLabelValues("regen")
	return m
}

// handler serves /metrics and /healthz.
func (m *Metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// Serve listens on address now, so that a port in use fails the start, and serves
// until the context is done.
func (m *Metrics) Serve(ctx context.Context, address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	server := &http.Server{Handler: m.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("Serving metrics", "address", listener.Addr().String())
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Metrics server failed", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	return nil
}
