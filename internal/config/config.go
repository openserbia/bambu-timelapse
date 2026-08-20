// Package config reads the service's environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults for every tunable. Named so the zero-config behaviour is readable
// in one place rather than scattered through Load as literals.
const (
	defaultFPS            = 20
	defaultMinFrames      = 30
	defaultFinalDelaySecs = 45
	defaultCaptureSecs    = 25
	defaultCaptureDelay   = 0
	defaultMinFreeMB      = 2048
	defaultFailedTTLDays  = 7

	mib = 1024 * 1024
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Host       string
	Serial     string
	AccessCode string
	// PrinterName is an operator alias used in the posted filename. Never the
	// serial: that is the MQTT topic key and the cloud-binding identifier, and
	// the filename is published to a Telegram channel.
	PrinterName string

	// APIURL and APIToken are the only thing this service knows about its
	// destination: an endpoint that accepts a multipart video.
	APIURL   string
	APIToken string
	// APIFields are extra form fields posted verbatim alongside the video.
	//
	// Routing belongs to whatever consumes the video, not here — a chat id, a
	// topic, a notification preference are all facts about the consumer. The
	// service passes them through without interpreting them, so it stays a
	// timelapse recorder rather than a Telegram client.
	APIFields map[string]string

	StagingDir string
	FPS        int
	// CaptureDelay is how long to wait after a layer change before grabbing.
	// A frame taken the instant layer_num increments catches the toolhead
	// mid-Z-hop, usually dead centre; a second or two later it has moved on.
	CaptureDelay time.Duration
	// Crop is an ffmpeg crop spec, "w:h:x:y", applied to the encoded video and
	// the cover. Empty leaves the frame whole. The gantry occupies a fixed
	// band at the top, so cropping it away is the cheapest way to remove the
	// toolhead entirely — at the cost of the top of tall prints.
	Crop           string
	MinFrames      int
	FinalDelay     time.Duration
	CaptureTimeout time.Duration
	MinFree        uint64
	FailedTTL      time.Duration
	ListenAddr     string
}

// Load reads the full runtime configuration.
func Load() (*Config, error) { return load(true) }

// LoadPrinter reads only what is needed to talk to the printer.
//
// The debug command has no destination to publish to, and demanding
// MEDIA_API_URL before it will tell you why the printer is unreachable gets
// the diagnostic order exactly backwards.
func LoadPrinter() (*Config, error) { return load(false) }

// load returns every problem at once rather than the first: a misconfigured
// deploy should need one restart to diagnose, not five.
func load(needSink bool) (*Config, error) {
	var errs []error
	req := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	num := func(key string, def int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a number", key, raw))
			return def
		}
		return n
	}
	// sink fields are required only when the service will actually publish.
	sink := func(key string) string {
		if needSink {
			return req(key)
		}
		return strings.TrimSpace(os.Getenv(key))
	}
	str := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := &Config{
		Host:           req("PRINTER_HOST"),
		Serial:         req("PRINTER_SERIAL"),
		AccessCode:     req("PRINTER_ACCESS_CODE"),
		PrinterName:    str("PRINTER_NAME", "printer"),
		APIURL:         sink("MEDIA_API_URL"),
		APIToken:       sink("MEDIA_API_TOKEN"),
		StagingDir:     str("STAGING_DIR", "/staging"),
		FPS:            num("TIMELAPSE_FPS", defaultFPS),
		CaptureDelay:   time.Duration(num("CAPTURE_DELAY", defaultCaptureDelay)) * time.Second,
		Crop:           str("CROP", ""),
		MinFrames:      num("MIN_FRAMES", defaultMinFrames),
		FinalDelay:     time.Duration(num("FINAL_FRAME_DELAY", defaultFinalDelaySecs)) * time.Second,
		CaptureTimeout: time.Duration(num("CAPTURE_TIMEOUT", defaultCaptureSecs)) * time.Second,
		MinFree:        megabytes(num("MIN_FREE_MB", defaultMinFreeMB)),
		FailedTTL:      time.Duration(num("FAILED_TTL_DAYS", defaultFailedTTLDays)) * 24 * time.Hour,
		ListenAddr:     str("LISTEN_ADDR", ":8092"),
	}

	if raw := strings.TrimSpace(os.Getenv("MEDIA_API_FIELDS")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.APIFields); err != nil {
			errs = append(errs, fmt.Errorf("MEDIA_API_FIELDS: not a JSON object of strings: %w", err))
		}
	}
	if cfg.FPS <= 0 {
		errs = append(errs, errors.New("TIMELAPSE_FPS must be positive"))
	}
	if cfg.CaptureDelay < 0 {
		errs = append(errs, errors.New("CAPTURE_DELAY must not be negative"))
	}
	// Validate the crop HERE rather than letting ffmpeg reject it: the encode
	// runs once, at the end of a print, so a typo would otherwise surface
	// hours later with every frame already captured and nothing to show.
	if cfg.Crop != "" && !cropSpec.MatchString(cfg.Crop) {
		errs = append(errs, fmt.Errorf("CROP: %q is not w:h:x:y (e.g. 1920:820:0:260)", cfg.Crop))
	}
	return cfg, errors.Join(errs...)
}

// cropSpec matches ffmpeg's numeric crop form. Expressions are legal to
// ffmpeg but rejected here: a config value that needs evaluating to check is a
// config value that fails at the worst possible moment.
var cropSpec = regexp.MustCompile(`^\d+:\d+:\d+:\d+$`)

// megabytes converts a configured MB value to bytes, clamping a negative
// setting to zero rather than wrapping it into a huge uint64 — a typo'd
// "-1" would otherwise disable capture entirely by making every disk look
// full.
func megabytes(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n) * mib
}
