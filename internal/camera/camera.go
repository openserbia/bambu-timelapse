// Package camera drives ffmpeg against the printer's RTSPS chamber stream.
package camera

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// rtspsPort is the printer's LAN live-view port. Only open once "LAN Only
	// Liveview" is enabled; until then rtsp_url in the MQTT ipcam section
	// reads the literal string "disable".
	rtspsPort = "322"
	// grabErrBytes / encodeErrBytes bound how much ffmpeg stderr is quoted
	// into an error; ffmpeg is verbose and the first lines carry the cause.
	grabErrBytes   = 300
	encodeErrBytes = 500
	// dimensionParts is the width/height pair ffprobe prints as "1920x1080".
	dimensionParts = 2
)

// Camera grabs stills from the printer's live view.
//
// One RTSPS session per frame, deliberately. The stream is 1080p MJPEG at
// 25 fps: holding it open burns bandwidth continuously between the layer
// changes we actually want, and the printer serves a limited number of
// viewers, so a persistent grab locks Bambu Studio out of its own camera.
// A single grab measures ~1.9s against a P2S, which is nothing next to a
// layer time.
type Camera struct {
	url     string
	timeout time.Duration
}

// New builds a Camera for the given printer.
func New(host, accessCode string, timeout time.Duration) *Camera {
	return &Camera{
		url: fmt.Sprintf("rtsps://bblp:%s@%s/streaming/live/1",
			url.QueryEscape(accessCode), net.JoinHostPort(host, rtspsPort)),
		timeout: timeout,
	}
}

// Grab writes one JPEG to dest.
func (c *Camera) Grab(ctx context.Context, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// G204: every argument is operator configuration read from the environment
	// at startup, not user input. The URL is assembled from the configured
	// host and access code; nothing here crosses a trust boundary.
	//nolint:gosec // see above
	cmd := exec.CommandContext(
		ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-rtsp_transport", "tcp",
		// The printer presents a self-signed certificate for its own LAN IP.
		"-tls_verify", "0",
		"-i", c.url,
		"-frames:v", "1", "-q:v", "2",
		// -update is mandatory for a single still: without it the image2 muxer
		// demands a %03d sequence pattern and errors, while confusingly still
		// writing a file.
		"-update", "1",
		dest,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("grab: %w: %s", err, truncate(string(out), grabErrBytes))
	}
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(dest)
		return errors.New("grab produced no frame")
	}
	return nil
}

// Encode muxes the numbered frames in dir into an H.264 mp4.
func Encode(ctx context.Context, dir, out string, fps int) error {
	// G204: dir and out are paths this service created inside its own staging
	// tree.
	//nolint:gosec // see above
	cmd := exec.CommandContext(
		ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.Itoa(fps),
		"-i", filepath.Join(dir, "frame-%05d.jpg"),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		// yuv420p for player compatibility. +faststart moves the moov atom to
		// the front, which is what lets Telegram render an inline player with
		// a poster frame instead of a grey file row.
		"-pix_fmt", "yuv420p", "-movflags", "+faststart",
		out,
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("encode: %w: %s", err, truncate(string(combined), encodeErrBytes))
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		return errors.New("encode produced no file")
	}
	return nil
}

// Dimensions probes a video's width and height, returning zeroes when ffprobe
// cannot tell — the media API treats 0 as "unknown" rather than an error.
func Dimensions(ctx context.Context, path string) (width, height int) {
	cmd := exec.CommandContext(ctx, "ffprobe", //nolint:gosec // G204: path is a file this service just encoded
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "x")
	if len(parts) != dimensionParts {
		return 0, 0
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	return w, h
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
