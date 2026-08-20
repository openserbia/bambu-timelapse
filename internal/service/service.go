// Package service wires telemetry, capture, encoding and upload together.
package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/openserbia/bambu-timelapse/internal/camera"
	"github.com/openserbia/bambu-timelapse/internal/config"
	"github.com/openserbia/bambu-timelapse/internal/session"
	"github.com/openserbia/bambu-timelapse/internal/telemetry"
	"github.com/openserbia/bambu-timelapse/internal/uploader"
)

const (
	// uploadAttempts is how many times a retryable upload failure is retried
	// before the job is parked for inspection.
	uploadAttempts = 3

	// MQTT connection tuning. The printer is on the LAN and reports
	// continuously, so a lost session should be re-established quickly but
	// back off rather than hammer a powered-off printer.
	connectRetryInterval = 10 * time.Second
	maxReconnectInterval = 60 * time.Second
	mqttKeepAlive        = 30 * time.Second
	disconnectGraceMS    = 2000

	// captureDrainLimit bounds how long finalisation waits for an in-flight
	// grab; a stuck ffmpeg must not hold up the post forever.
	captureDrainLimit = 10 * time.Second

	// Filename component limits, matching what the media API enforces.
	printerNameLimit = 20
	jobNameLimit     = 60

	// mqttPort is the printer's TLS MQTT listener.
	mqttPort = "8883"

	// mibShift converts bytes to MiB for log lines.
	mibShift = 20
	// captionPerm keeps the caption file owner-only, like the rest of staging.
	captionPerm = 0o600
)

// Service is the running daemon.
type Service struct {
	cfg   *config.Config
	log   *slog.Logger
	state *telemetry.State
	store *session.Store
	cam   *camera.Camera
	up    *uploader.Uploader
	m     *Metrics

	mu      sync.Mutex
	current *session.Session
	// reconciled guards the startup pass so it runs once, on the first
	// snapshot that actually carries a printer state.
	reconciled bool

	capturing   atomic.Bool
	lastReport  atomic.Int64
	mqttUp      atomic.Bool
	finalizing  sync.WaitGroup
	shutdownCtx context.Context
}

// New builds the service.
func New(cfg *config.Config, log *slog.Logger) (*Service, error) {
	store, err := session.NewStore(cfg.StagingDir)
	if err != nil {
		return nil, fmt.Errorf("staging: %w", err)
	}
	return &Service{
		cfg:   cfg,
		log:   log,
		state: telemetry.NewState(),
		store: store,
		cam:   camera.New(cfg.Host, cfg.AccessCode, cfg.CaptureTimeout),
		up:    uploader.New(cfg.APIURL, cfg.APIToken, cfg.APIFields),
		m:     NewMetrics(),
	}, nil
}

// Run connects to the printer and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	s.shutdownCtx = ctx

	if n := s.store.SweepFailed(s.cfg.FailedTTL); n > 0 {
		s.log.Info("swept expired failed jobs", "count", n)
	}
	go s.retryParked(ctx)

	opts := mqtt.NewClientOptions().
		AddBroker("ssl://" + net.JoinHostPort(s.cfg.Host, mqttPort)).
		SetClientID(fmt.Sprintf("bambu-timelapse-%d", time.Now().UnixNano())).
		SetUsername("bblp").
		SetPassword(s.cfg.AccessCode).
		// The printer presents a self-signed certificate for its own LAN IP.
		SetTLSConfig(insecureTLS()).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(connectRetryInterval).
		SetMaxReconnectInterval(maxReconnectInterval).
		SetKeepAlive(mqttKeepAlive).
		// Sequential delivery: onMessage merges into shared state and trips the
		// one-shot reconcile latch. Concurrent callbacks would race both.
		SetOrderMatters(true)

	opts.OnConnect = s.onConnect
	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		s.mqttUp.Store(false)
		s.m.MQTTUp.Set(0)
		s.log.Warn("mqtt connection lost", "err", err)
	}

	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.Wait()
	if err := token.Error(); err != nil {
		// Not fatal: SetConnectRetry keeps trying, and a printer that is simply
		// powered off should not crash-loop the container.
		s.log.Error("initial mqtt connect failed; retrying in background", "err", err)
	}

	<-ctx.Done()
	s.log.Info("shutting down; waiting for in-flight finalisation")
	client.Disconnect(disconnectGraceMS)
	s.finalizing.Wait()
	return nil
}

func (s *Service) onConnect(client mqtt.Client) {
	s.mqttUp.Store(true)
	s.m.MQTTUp.Set(1)

	topic := fmt.Sprintf("device/%s/report", s.cfg.Serial)
	if token := client.Subscribe(topic, 0, s.onMessage); token.Wait() && token.Error() != nil {
		s.log.Error("subscribe failed", "topic", topic, "err", token.Error())
		return
	}
	s.log.Info("connected", "topic", topic)

	// Ask for a full snapshot immediately. Reports are deltas and a full one
	// arrives only every 20-55s, so without this a reconnect leaves the service
	// blind for up to a minute — exactly when it most needs to know whether a
	// job is still running and which one.
	req := fmt.Sprintf("device/%s/request", s.cfg.Serial)
	payload := `{"pushing":{"sequence_id":"0","command":"pushall"}}`
	if token := client.Publish(req, 0, false, payload); token.Wait() && token.Error() != nil {
		s.log.Warn("pushall request failed; falling back to periodic reports",
			"err", token.Error())
	}
}

func (s *Service) onMessage(_ mqtt.Client, msg mqtt.Message) {
	s.lastReport.Store(time.Now().Unix())
	if !s.state.Merge(msg.Payload()) {
		return
	}
	s.observe()

	if !s.reconciled && s.state.GcodeState() != "" {
		s.reconciled = true
		s.reconcile()
	}
	s.handle()
}

// observe refreshes the gauges that mirror printer state.
func (s *Service) observe() {
	s.m.Layer.Set(float64(s.state.Layer()))
	s.m.TotalLayers.Set(float64(s.state.TotalLayers()))
	s.m.Progress.Set(float64(s.state.Progress()))
	s.m.Nozzle.Set(s.state.Nozzle())
	s.m.Bed.Set(s.state.Bed())
	s.m.SetState(s.state.GcodeState())
}

// reconcile settles what to do with sessions left on disk by a previous
// process — a crash, a reboot, or a container restart mid-print.
func (s *Service) reconcile() {
	live := s.state.TaskID()
	running := s.state.GcodeState() == telemetry.StateRunning || s.state.GcodeState() == telemetry.StatePause

	sessions, err := s.store.List()
	if err != nil {
		s.log.Error("cannot list staging", "err", err)
		return
	}
	for _, sess := range sessions {
		switch {
		case running && sess.TaskID == live:
			// Same job still printing: adopt it. Keeping StartedAt from disk
			// is what keeps the caption's duration honest across a restart.
			s.mu.Lock()
			s.current = sess
			s.mu.Unlock()
			s.m.SessionFrames.Set(float64(sess.Frames))
			s.log.Info("resumed session after restart",
				"task_id", sess.TaskID, "frames", sess.Frames,
				"last_layer", sess.LastLayer, "started_at", sess.StartedAt)
		default:
			// Either the printer moved on to another job or it is idle: this
			// one ended while we were down. Post what was captured rather than
			// throw away hours of frames.
			s.log.Info("finalising a session left by a previous run",
				"task_id", sess.TaskID, "frames", sess.Frames)
			s.startFinalize(sess, telemetry.StateFinish, true)
		}
	}
}

// retryParked re-attempts uploads that were parked by an earlier run, so an
// AX41 outage during the night resolves itself rather than needing a human.
func (s *Service) retryParked(ctx context.Context) {
	entries, err := os.ReadDir(s.store.FailedDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "upload-failed-") {
			continue
		}
		dir := filepath.Join(s.store.FailedDir(), e.Name())
		video := findVideo(dir)
		if video == "" {
			continue
		}
		s.log.Info("retrying a parked upload", "dir", e.Name())
		cover := filepath.Join(dir, "cover.jpg")
		if _, err := os.Stat(cover); err != nil {
			cover = ""
		}
		w, h := camera.Dimensions(ctx, video)
		req := uploader.Request{
			VideoPath: video, CoverPath: cover,
			Filename: filepath.Base(video),
			Caption:  readCaption(dir),
			Width:    w, Height: h,
		}
		if ids, err := s.up.Post(ctx, req); err == nil {
			s.log.Info("parked upload succeeded", "ids", ids)
			s.m.Uploads.WithLabelValues("ok").Inc()
			_ = os.RemoveAll(dir)
		}
	}
}

// handle drives the job lifecycle from the merged state.
func (s *Service) handle() {
	st := s.state.GcodeState()

	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()

	switch st {
	case telemetry.StateRunning:
		if cur == nil {
			cur = s.begin()
			if cur == nil {
				return
			}
		}
		s.mu.Lock()
		s.record(cur)
		layer := s.state.Layer()
		newLayer := layer > cur.LastLayer
		if newLayer {
			cur.LastLayer = layer
			_ = cur.Save()
		}
		s.mu.Unlock()
		if newLayer {
			s.capture(cur, layer)
		}
	case telemetry.StatePause:
		// Nothing to do: pauses are counted from the transition, and the
		// camera keeps its last frame.
	case telemetry.StateFinish, telemetry.StateFailed, telemetry.StateIdle:
		if cur == nil {
			return
		}
		s.mu.Lock()
		s.current = nil
		s.mu.Unlock()
		s.startFinalize(cur, st, false)
	}
}

func (s *Service) begin() *session.Session {
	taskID := s.state.TaskID()
	if taskID == "" {
		// Without an identity there is no safe resume key, and a restart would
		// silently start a second session for the same print.
		s.log.Warn("printer is RUNNING but reports no task id; waiting")
		return nil
	}
	sess, resumed, err := s.store.Create(taskID, s.state.JobName(),
		s.state.TotalLayers(), s.state.Layer())
	if err != nil {
		s.log.Error("cannot open session", "err", err)
		return nil
	}
	s.mu.Lock()
	s.current = sess
	s.mu.Unlock()
	s.log.Info("job started",
		"task_id", taskID, "job", sess.JobName,
		"layers", sess.TotalLayers, "resumed", resumed, "partial", sess.Partial)
	return sess
}

func (s *Service) record(sess *session.Session) {
	sess.Nozzle.Add(s.state.Nozzle())
	sess.Bed.Add(s.state.Bed())
	if sess.Filament == "" {
		sess.Filament = s.state.Filament()
	}
	if sess.TotalLayers == 0 {
		sess.TotalLayers = s.state.TotalLayers()
	}
}

func (s *Service) capture(sess *session.Session, layer int) {
	if s.capturing.Load() {
		// A grab takes ~2s and fast layers can outrun it. Skipping is right:
		// blocking would stall the MQTT callback, and queueing would capture a
		// layer that has already been printed over.
		s.log.Debug("skipping layer; capture in flight", "layer", layer)
		return
	}
	if free, err := freeBytes(s.store.Root()); err == nil && free < s.cfg.MinFree {
		s.log.Warn("below the free-space floor; not capturing", "free_mb", free>>mibShift)
		return
	}
	s.capturing.Store(true)
	go func() {
		defer s.capturing.Store(false)

		// Wait before shooting: the instant layer_num increments the toolhead
		// is mid-Z-hop and usually dead centre, so a frame taken right then is
		// the worst one available. Held in the capture goroutine, never in the
		// MQTT callback.
		if s.cfg.CaptureDelay > 0 {
			select {
			case <-time.After(s.cfg.CaptureDelay):
			case <-s.shutdownCtx.Done():
				return
			}
		}

		dest := sess.FramePath()
		started := time.Now()
		if err := s.cam.Grab(s.shutdownCtx, dest); err != nil {
			s.log.Warn("frame grab failed", "layer", layer, "err", err)
			s.m.CaptureFailures.Inc()
			return
		}
		s.mu.Lock()
		sess.Frames++
		_ = sess.Save()
		frames := sess.Frames
		s.mu.Unlock()
		s.m.FramesCaptured.Inc()
		s.m.SessionFrames.Set(float64(frames))
		s.log.Info("frame captured", "n", frames, "layer", layer,
			"took", time.Since(started).Round(time.Millisecond))
	}()
}

func (s *Service) startFinalize(sess *session.Session, endState string, skipDelay bool) {
	s.finalizing.Add(1)
	go func() {
		defer s.finalizing.Done()
		s.finalize(sess, endState, skipDelay)
	}()
}

func (s *Service) finalize(sess *session.Session, endState string, skipDelay bool) {
	// A grab may still be writing frame N+1. Encoding while it lands would
	// either miss it or read a half-written JPEG, so drain first.
	s.awaitCapture(captureDrainLimit)

	elapsed := time.Since(sess.StartedAt)
	s.log.Info("job ended", "state", endState, "frames", sess.Frames,
		"elapsed", elapsed.Round(time.Second))

	ctx := context.WithoutCancel(s.shutdownCtx)

	cover := s.coverFrame(ctx, sess, skipDelay)

	if sess.Frames < s.cfg.MinFrames {
		s.log.Info("too few frames; discarding",
			"frames", sess.Frames, "min", s.cfg.MinFrames)
		s.m.Uploads.WithLabelValues("skipped").Inc()
		s.store.Discard(sess)
		return
	}

	filename := s.filename(sess)
	video := filepath.Join(sess.Dir(), filename)
	if err := camera.Encode(ctx, sess.Dir(), video, s.cfg.FPS, s.cfg.Crop); err != nil {
		s.log.Error("encode failed", "err", err)
		s.m.Uploads.WithLabelValues("failed").Inc()
		_ = s.store.Park(sess, "encode-failed")
		return
	}

	caption := s.caption(sess, endState, elapsed)
	_ = os.WriteFile(filepath.Join(sess.Dir(), "caption.txt"), []byte(caption), captionPerm)

	w, h := camera.Dimensions(ctx, video)
	req := uploader.Request{
		VideoPath: video, CoverPath: cover, Filename: filename, Caption: caption,
		Duration: max(1, sess.Frames/s.cfg.FPS), Width: w, Height: h,
	}

	for attempt := 1; attempt <= uploadAttempts; attempt++ {
		ids, err := s.up.Post(ctx, req)
		if err == nil {
			s.log.Info("posted", "file", filename, "ids", ids)
			s.m.Uploads.WithLabelValues("ok").Inc()
			s.m.LastUpload.SetToCurrentTime()
			// The 200 IS the delete token: the endpoint is synchronous and only
			// answers once Telegram holds the bytes.
			s.store.Discard(sess)
			return
		}
		s.log.Error("upload failed", "attempt", attempt, "err", err)
		var apiErr *uploader.Error
		if errors.As(err, &apiErr) && !apiErr.Retryable {
			break
		}
		select {
		case <-time.After(time.Duration(5<<attempt) * time.Second):
		case <-s.shutdownCtx.Done():
			// Park rather than keep retrying through a shutdown; the next boot
			// picks it up from failed/.
		}
	}
	s.m.Uploads.WithLabelValues("failed").Inc()
	_ = s.store.Park(sess, "upload-failed")
}

// coverFrame produces the poster image: a fresh grab once the bed has dropped
// and the head parked, falling back to the last captured frame, cropped to
// match the footage it introduces. Returns "" when there is nothing usable.
func (s *Service) coverFrame(ctx context.Context, sess *session.Session, skipDelay bool) string {
	lastFrame := filepath.Join(sess.Dir(), fmt.Sprintf("frame-%05d.jpg", sess.Frames))
	cover := filepath.Join(sess.Dir(), "cover.jpg")

	if skipDelay {
		// Recovered from disk: the print ended long ago, so there is nothing
		// to wait for and the camera now shows an empty or reloaded bed.
		cover = lastFrame
	} else {
		select {
		case <-time.After(s.cfg.FinalDelay):
		case <-s.shutdownCtx.Done():
		}
		if err := s.cam.Grab(ctx, cover); err != nil {
			s.log.Warn("final grab failed; using the last frame", "err", err)
			cover = lastFrame
		}
	}

	if _, err := os.Stat(cover); err != nil {
		return ""
	}
	if s.cfg.Crop == "" {
		return cover
	}

	cropped := filepath.Join(sess.Dir(), "cover-cropped.jpg")
	if err := camera.Crop(ctx, cover, cropped, s.cfg.Crop); err != nil {
		// A mismatched cover is cosmetic; a missing video is not.
		s.log.Warn("cover crop failed; using the uncropped frame", "err", err)
		return cover
	}
	return cropped
}

// awaitCapture blocks until no grab is in flight, or the deadline passes —
// a stuck ffmpeg must not hold up the post forever.
func (s *Service) awaitCapture(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for s.capturing.Load() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Service) filename(sess *session.Session) string {
	stamp := sess.StartedAt.Format("2006-01-02T15-04")
	return fmt.Sprintf("%s_%s_%s.mp4",
		clean(s.cfg.PrinterName, printerNameLimit), clean(sess.JobName, jobNameLimit), stamp)
}

func (s *Service) caption(sess *session.Session, endState string, elapsed time.Duration) string {
	icon := "✅"
	if endState != telemetry.StateFinish {
		icon = "❌"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>\n", icon, html.EscapeString(sess.JobName))
	fmt.Fprintf(&b, "%s · %s", html.EscapeString(s.cfg.PrinterName), humanDuration(elapsed))
	if sess.Partial {
		// Never present a partial capture's elapsed time as the print duration:
		// the video would visibly contradict it.
		b.WriteString(" (capture started mid-print)")
	}
	if sess.TotalLayers > 0 {
		fmt.Fprintf(&b, " · %d/%d layers", sess.LastLayer, sess.TotalLayers)
	}
	if sess.Filament != "" {
		fmt.Fprintf(&b, "\nFilament: %s", html.EscapeString(sess.Filament))
	}
	if sess.Nozzle.Count > 0 {
		fmt.Fprintf(&b, "\nNozzle %.0f°C avg / %.0f peak · Bed %.0f°C",
			sess.Nozzle.Avg(), sess.Nozzle.Peak, sess.Bed.Avg())
	}
	if sess.Pauses > 0 {
		fmt.Fprintf(&b, "\n%d pause(s)", sess.Pauses)
	}
	// No filament weight: the printer reports only coarse AMS remaining
	// percentages, and the slicer's gram estimate lives in the 3MF, which this
	// pipeline never reads.
	return b.String()
}
