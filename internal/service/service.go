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
	tools camera.Tools
	up    *uploader.Uploader
	m     *Metrics

	mu      sync.Mutex
	current *session.Session
	// reconciled guards the startup pass so it runs once, on the first
	// snapshot that actually carries a printer state.
	reconciled bool

	// font is the path drawtext draws with, resolved once at startup. Empty
	// means captions are off for this run.
	font string

	// stop ends Run early, which is how a one-shot recording finishes after
	// the print it was started for.
	stop context.CancelFunc

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
	tools := camera.NewTools(cfg.FFmpegBin, cfg.FFprobeBin)
	svc := &Service{
		cfg:   cfg,
		tools: tools,
		log:   log,
		state: telemetry.NewState(),
		store: store,
		cam:   camera.New(cfg.Host, cfg.AccessCode, cfg.CaptureTimeout, tools),
		up:    uploader.New(cfg.APIURL, cfg.APIToken, cfg.APIFields),
		m:     NewMetrics(),
	}
	if cfg.Overlay {
		svc.font = svc.resolveFont()
	}
	return svc, nil
}

// resolveFont settles on a font once, at startup, rather than at the encode —
// which runs once, at the end of a print. Failing to find one costs the
// caption and says so; it is never a reason not to record the print.
func (s *Service) resolveFont() string {
	if s.cfg.OverlayFont != "" {
		_, err := os.Stat(s.cfg.OverlayFont)
		if err == nil {
			return s.cfg.OverlayFont
		}
		s.log.Warn("OVERLAY_FONT is unreadable; using the bundled font",
			"path", s.cfg.OverlayFont, "err", err)
	}
	font, err := camera.BundledFont(s.store.Root())
	if err != nil {
		s.log.Warn("no font to draw with; captions are off for this run", "err", err)
		return ""
	}
	return font
}

// RunTimed captures on a clock rather than on layer changes, and never speaks
// to the printer's telemetry at all.
//
// It exists because the interesting parts of this service — the crop, the
// caption, the held ends, the encode — are testable in a minute, while a
// layer-synced capture needs a print. An idle printer still serves its camera.
// The result is deliberately not layer synced and carries no layer counter:
// there are no layers, and a caption that implied otherwise would be a claim
// the video contradicts.
func (s *Service) RunTimed(ctx context.Context, every time.Duration, maxFrames int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.stop = cancel
	s.shutdownCtx = ctx

	if err := s.preflight(ctx); err != nil {
		return err
	}

	// A synthetic identity: the resume key exists to survive restarts, and a
	// timed capture that outlives one is not something anyone wants resumed.
	taskID := fmt.Sprintf("timed-%d", time.Now().Unix())
	sess, _, err := s.store.Create(taskID, "timed capture", 0, 0)
	if err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	s.log.Info("capturing on a timer",
		"every", every, "frames", maxFrames, "dir", sess.Dir())

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for maxFrames <= 0 || sess.Frames < maxFrames {
		select {
		case <-ctx.Done():
			s.log.Info("stopping; encoding what was captured", "frames", sess.Frames)
			s.finalize(sess, telemetry.StateFinish, true)
			return nil
		case <-ticker.C:
		}

		dest := sess.FramePath()
		if err := s.cam.Grab(ctx, dest); err != nil {
			// Not fatal: one refused RTSPS session should not end a capture
			// that is otherwise working.
			s.log.Warn("frame grab failed", "err", err)
			s.m.CaptureFailures.Inc()
			continue
		}
		sess.Frames++
		_ = sess.Save()
		s.m.FramesCaptured.Inc()
		s.m.SessionFrames.Set(float64(sess.Frames))
		s.log.Info("frame captured", "n", sess.Frames)
	}

	s.finalize(sess, telemetry.StateFinish, true)
	return nil
}

// Run connects to the printer and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.stop = cancel
	s.shutdownCtx = ctx

	// Before anything is captured: a print runs for hours, and finding out at
	// the encode that ffmpeg was never installed wastes all of them.
	if err := s.preflight(ctx); err != nil {
		return err
	}

	if n := s.store.SweepFailed(s.cfg.FailedTTL); n > 0 {
		s.log.Info("swept expired failed jobs", "count", n)
	}
	if s.cfg.APIURL != "" {
		go s.retryParked(ctx)
	}

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
		w, h := s.tools.Dimensions(ctx, video)
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
		if cur != nil && cur.TaskID != s.state.TaskID() && s.state.TaskID() != "" {
			// A new print without a state we ever saw end: the printer went
			// straight from one job to the next. Post what the old one got
			// rather than let the new one's frames land in its directory.
			s.log.Info("a new job started while one was open",
				"was", cur.TaskID, "now", s.state.TaskID(), "frames", cur.Frames)
			s.mu.Lock()
			s.current = nil
			s.mu.Unlock()
			s.startFinalize(cur, telemetry.StateFinish, true)
			cur = nil
		}
		if cur == nil {
			cur = s.begin()
			if cur == nil {
				return
			}
		}
		s.mu.Lock()
		s.record(cur)
		layer := s.state.Layer()
		newLayer := changedLayer(layer, cur.LastLayer)
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

// changedLayer decides whether this report is worth a frame.
//
// Any change counts, not just an increase. Layer numbers only climb within a
// job, so a lower one means the number belongs to a different print — and
// requiring an increase there is what let one job's stale count lock out the
// whole of the next one.
func changedLayer(layer, last int) bool {
	return layer != last && layer > 0
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
		sess.Layers = append(sess.Layers, layer)
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
	if err := s.encode(ctx, sess, video, cover); err != nil {
		s.log.Error("encode failed", "err", err)
		s.m.Uploads.WithLabelValues("failed").Inc()
		_ = s.store.Park(sess, "encode-failed")
		return
	}

	caption := s.caption(sess, endState, elapsed)
	_ = os.WriteFile(filepath.Join(sess.Dir(), "caption.txt"), []byte(caption), captionPerm)

	if s.cfg.APIURL == "" {
		s.deliverLocally(sess, video, cover, caption, filename)
		return
	}

	w, h := s.tools.Dimensions(ctx, video)
	req := uploader.Request{
		VideoPath: video, CoverPath: cover, Filename: filename, Caption: caption,
		Duration: s.duration(sess, cover), Width: w, Height: h,
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

// deliverLocally keeps the recording instead of posting it, which is what
// `record` is for: checking a crop or a caption should not need a chat on the
// other end, or put a test print in one.
func (s *Service) deliverLocally(sess *session.Session, video, cover, caption, filename string) {
	out := s.cfg.OutputDir
	if err := os.MkdirAll(out, outputDirPerm); err != nil {
		s.log.Error("cannot write to the output directory; leaving the job staged",
			"dir", out, "err", err)
		_ = s.store.Park(sess, "output-unwritable")
		return
	}

	dest := filepath.Join(out, filename)
	if err := move(video, dest); err != nil {
		s.log.Error("cannot save the video; leaving the job staged", "err", err)
		_ = s.store.Park(sess, "output-unwritable")
		return
	}
	base := strings.TrimSuffix(dest, filepath.Ext(dest))
	if cover != "" {
		if err := move(cover, base+"-cover.jpg"); err != nil {
			s.log.Warn("cover not saved", "err", err)
		}
	}
	// The caption alongside the video rather than inside it: it is what the
	// posted message would have said, and reading it is how you check it.
	if err := os.WriteFile(base+".txt", []byte(caption), captionPerm); err != nil {
		s.log.Warn("caption not saved", "err", err)
	}

	s.log.Info("recorded", "file", dest, "frames", sess.Frames)
	s.m.Uploads.WithLabelValues("local").Inc()
	s.store.Discard(sess)

	if s.cfg.Once && s.stop != nil {
		s.log.Info("one print recorded; stopping")
		s.stop()
	}
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
	if err := s.tools.Crop(ctx, cover, cropped, s.cfg.Crop); err != nil {
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

// encode renders the video, and if that fails with a caption, renders it
// again without one.
//
// The overlay is decoration; the footage is a print that took hours and
// cannot be repeated. Parking a finished job over a drawtext argument would
// be the wrong way round.
func (s *Service) encode(ctx context.Context, sess *session.Session, video, cover string) error {
	opts := camera.EncodeOptions{
		FPS:     s.cfg.FPS,
		Crop:    s.cfg.Crop,
		Cover:   cover,
		Intro:   s.cfg.Intro,
		Tail:    s.cfg.Tail,
		Overlay: s.overlay(sess),
	}
	err := s.tools.Encode(ctx, sess.Dir(), video, opts)
	if err == nil || opts.Overlay == nil {
		return err
	}
	s.log.Error("encode failed; retrying without the caption", "err", err)
	opts.Overlay = nil
	return s.tools.Encode(ctx, sess.Dir(), video, opts)
}

// overlay builds the caption burned into the footage: a fixed title line and,
// under it, the layer the frame on screen was actually captured on.
func (s *Service) overlay(sess *session.Session) *camera.Overlay {
	if !s.cfg.Overlay {
		return nil
	}
	if s.font == "" {
		return nil
	}
	o := &camera.Overlay{FontFile: s.font, Title: s.cfg.PrinterName}
	if sess.JobName != "" {
		o.Title += " · " + sess.JobName
	}
	// A state file written before the layer list existed cannot say which
	// layer any given frame belongs to, and a counter that is merely plausible
	// is worse than no counter: the video contradicts it on the way past.
	if len(sess.Layers) != sess.Frames {
		return o
	}
	o.Lines = make([]string, sess.Frames)
	for i, layer := range sess.Layers {
		if sess.TotalLayers > 0 {
			o.Lines[i] = fmt.Sprintf("Layer %d/%d", layer, sess.TotalLayers)
			continue
		}
		o.Lines[i] = fmt.Sprintf("Layer %d", layer)
	}
	return o
}

// duration is what the destination is told the video runs for: the footage
// plus whatever is held at either end, so a player's scrubber matches the
// file it is scrubbing.
func (s *Service) duration(sess *session.Session, cover string) int {
	total := time.Duration(sess.Frames) * time.Second / time.Duration(s.cfg.FPS)
	total += s.cfg.Tail
	if cover != "" {
		total += s.cfg.Intro
	}
	return max(1, int(total.Seconds()))
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
