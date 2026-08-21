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

// Tools names the binaries this package runs.
//
// Configurable because "ffmpeg" on PATH is an assumption about the caller's
// environment, not a fact: an IDE's run configuration, a cron job and a
// container all have different PATHs, and the failure is a print recorded to
// nothing.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// NewTools fills in the plain names, which resolve through PATH as before.
func NewTools(ffmpegBin, ffprobeBin string) Tools {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if ffprobeBin == "" {
		ffprobeBin = "ffprobe"
	}
	return Tools{FFmpeg: ffmpegBin, FFprobe: ffprobeBin}
}

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
	tools   Tools
}

// New builds a Camera for the given printer.
func New(host, accessCode string, timeout time.Duration, tools Tools) *Camera {
	return &Camera{
		tools: tools,
		url: fmt.Sprintf("rtsps://bblp:%s@%s/streaming/live/1",
			url.QueryEscape(accessCode), net.JoinHostPort(host, rtspsPort)),
		timeout: timeout,
	}
}

// Grab writes one JPEG to dest.
func (c *Camera) Grab(ctx context.Context, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	cmd := c.tools.ffmpeg(
		ctx,
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

// Crop rewrites an image with the same crop applied to the video, so the cover
// frame and the footage it introduces are framed identically.
func (t Tools) Crop(ctx context.Context, src, dst, crop string) error {
	if crop == "" {
		return nil
	}
	cmd := t.ffmpeg(ctx,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-vf", "crop="+crop, "-q:v", "2", "-update", "1", dst)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crop cover: %w: %s", err, truncate(string(combined), grabErrBytes))
	}
	return nil
}

// Dimensions probes a video's width and height, returning zeroes when ffprobe
// cannot tell. The media API treats 0 as "unknown" rather than an error.
func (t Tools) Dimensions(ctx context.Context, path string) (width, height int) {
	cmd := exec.CommandContext(ctx, t.FFprobe, //nolint:gosec // G204: the binary is operator configuration and the path is a file this service just encoded
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

// ffmpeg builds an invocation of the encoder.
//
// The G204 waiver lives here rather than at every call site: every argument
// this package passes is either a path inside the service's own staging tree
// or operator configuration validated at startup: the printer host, the
// access code, the crop, the font.
func (t Tools) ffmpeg(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, t.FFmpeg, args...) //nolint:gosec // see above
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
