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

	"github.com/joho/godotenv"
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
	defaultIntroSecs      = 5
	defaultTailSecs       = 5

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
	// OutputDir is where a local recording lands. Set only by the record
	// command; with it set and APIURL empty the service keeps what it makes
	// instead of posting it.
	OutputDir string
	// Once ends the run after the first print is written. A recording made to
	// check a crop or a caption should not leave a daemon behind.
	Once bool
	FPS  int
	// CaptureDelay is how long to wait after a layer change before grabbing.
	// A frame taken the instant layer_num increments catches the toolhead
	// mid-Z-hop, usually dead centre; a second or two later it has moved on.
	CaptureDelay time.Duration
	// Crop is an ffmpeg crop spec, "w:h:x:y", applied to the encoded video and
	// the cover. Empty leaves the frame whole. The gantry occupies a fixed
	// band at the top, so cropping it away is the cheapest way to remove the
	// toolhead entirely — at the cost of the top of tall prints.
	Crop string
	// Overlay burns the printer name, the job and a live layer counter into
	// the footage. The counter is driven by the layer each frame was actually
	// captured on, not by the frame index: a skipped grab leaves no frame, so
	// counting frames would drift a little further from the truth with every
	// layer the camera missed.
	Overlay bool
	// OverlayFont is an optional path to a font to draw with. Empty — the
	// default — uses the one compiled into the binary, so a caption never
	// depends on what the host happens to have installed.
	OverlayFont string
	// FFmpegBin and FFprobeBin name the binaries to run. Plain names resolve
	// through PATH; absolute paths are for the environments that have their
	// own idea of PATH — an IDE run configuration, a cron entry, a unit file.
	FFmpegBin  string
	FFprobeBin string
	// Intro holds the cover still at the head of the video and Tail holds the
	// last captured frame at the end, so the result opens on the finished
	// print and does not cut away the instant the print does.
	Intro          time.Duration
	Tail           time.Duration
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
	// A .env is how this runs on a laptop. In the container there is none and
	// the environment is the compose file's, so a missing file is not an
	// error — only a malformed one would be, and that surfaces as a missing
	// required variable below.
	_ = godotenv.Load()

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
	flag := func(key string, def bool) bool {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not true or false", key, raw))
			return def
		}
		return v
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
		OutputDir:      str("OUTPUT_DIR", ""),
		FPS:            num("TIMELAPSE_FPS", defaultFPS),
		CaptureDelay:   time.Duration(num("CAPTURE_DELAY", defaultCaptureDelay)) * time.Second,
		Crop:           str("CROP", ""),
		Overlay:        flag("OVERLAY", true),
		OverlayFont:    str("OVERLAY_FONT", ""),
		FFmpegBin:      str("FFMPEG_BIN", "ffmpeg"),
		FFprobeBin:     str("FFPROBE_BIN", "ffprobe"),
		Intro:          time.Duration(num("INTRO_HOLD", defaultIntroSecs)) * time.Second,
		Tail:           time.Duration(num("TAIL_HOLD", defaultTailSecs)) * time.Second,
		MinFrames:      num("MIN_FRAMES", defaultMinFrames),
		FinalDelay:     time.Duration(num("FINAL_FRAME_DELAY", defaultFinalDelaySecs)) * time.Second,
		CaptureTimeout: time.Duration(num("CAPTURE_TIMEOUT", defaultCaptureSecs)) * time.Second,
		MinFree:        megabytes(num("MIN_FREE_MB", defaultMinFreeMB)),
		FailedTTL:      time.Duration(num("FAILED_TTL_DAYS", defaultFailedTTLDays)) * 24 * time.Hour,
		ListenAddr:     str("LISTEN_ADDR", ":8092"),
	}

	return cfg, errors.Join(append(errs, validate(cfg)...)...)
}

// validate is separate from reading so that parsing an environment and
// judging it stay one concern each: everything here is a rule about the
// resolved configuration, not about how it was spelled.
func validate(cfg *Config) []error {
	var errs []error
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
	if cfg.Intro < 0 || cfg.Tail < 0 {
		errs = append(errs, errors.New("INTRO_HOLD and TAIL_HOLD must not be negative"))
	}
	// Validate the crop HERE rather than letting ffmpeg reject it: the encode
	// runs once, at the end of a print, so a typo would otherwise surface
	// hours later with every frame already captured and nothing to show.
	if cfg.Crop != "" && !cropSpec.MatchString(cfg.Crop) {
		errs = append(errs, fmt.Errorf("CROP: %q is not w:h:x:y (e.g. 1920:820:0:260)", cfg.Crop))
	}
	return errs
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
