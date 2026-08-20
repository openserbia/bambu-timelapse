package service

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/openserbia/bambu-timelapse/internal/telemetry"
)

// Metrics is the service's Prometheus surface.
//
// The counters exist because the interesting failures are silent: a service
// that is up but capturing nothing looks identical to an idle printer unless
// frames_captured is flat while a job runs.
type Metrics struct {
	MQTTUp          prometheus.Gauge
	FramesCaptured  prometheus.Counter
	CaptureFailures prometheus.Counter
	Uploads         *prometheus.CounterVec
	LastUpload      prometheus.Gauge
	Layer           prometheus.Gauge
	TotalLayers     prometheus.Gauge
	Progress        prometheus.Gauge
	Nozzle          prometheus.Gauge
	Bed             prometheus.Gauge
	SessionFrames   prometheus.Gauge
	PrintState      *prometheus.GaugeVec

	registry *prometheus.Registry
}

var knownStates = []string{
	telemetry.StateRunning, telemetry.StatePause, telemetry.StateFinish,
	telemetry.StateFailed, telemetry.StateIdle,
}

// NewMetrics builds and registers the metric set on a private registry, so
// /metrics carries this service's numbers and not the Go runtime's default
// collectors from every imported library.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
		reg.MustRegister(g)
		return g
	}
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
		reg.MustRegister(c)
		return c
	}

	m := &Metrics{
		MQTTUp:          gauge("bambu_mqtt_connected", "Printer MQTT session is up."),
		FramesCaptured:  counter("bambu_frames_captured_total", "Camera frames written."),
		CaptureFailures: counter("bambu_capture_failures_total", "Frame grabs that errored or timed out."),
		LastUpload:      gauge("bambu_last_upload_timestamp_seconds", "Unix time of the last successful post."),
		Layer:           gauge("bambu_layer_num", "Current layer."),
		TotalLayers:     gauge("bambu_total_layers", "Layers in the running job."),
		Progress:        gauge("bambu_progress_percent", "Print progress."),
		Nozzle:          gauge("bambu_nozzle_temperature_celsius", "Nozzle temperature."),
		Bed:             gauge("bambu_bed_temperature_celsius", "Bed temperature."),
		SessionFrames:   gauge("bambu_session_frames", "Frames held for the running job."),
		Uploads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bambu_uploads_total", Help: "Timelapse posts by result.",
		}, []string{"result"}),
		PrintState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bambu_print_state", Help: "Printer state, 1 on the active label.",
		}, []string{"state"}),
		registry: reg,
	}
	reg.MustRegister(m.Uploads, m.PrintState)

	// Pre-create every series so a dashboard has continuous lines from boot
	// rather than a gap until the first event of each kind.
	for _, r := range []string{"ok", "failed", "skipped"} {
		m.Uploads.WithLabelValues(r)
	}
	for _, st := range knownStates {
		m.PrintState.WithLabelValues(st).Set(0)
	}
	return m
}

// SetState raises the gauge for the active state and clears the rest.
func (m *Metrics) SetState(state string) {
	for _, st := range knownStates {
		v := 0.0
		if st == state {
			v = 1
		}
		m.PrintState.WithLabelValues(st).Set(v)
	}
}

// Registry exposes the private registry for the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
