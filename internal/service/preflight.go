package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openserbia/bambu-timelapse/internal/camera"
	"github.com/openserbia/bambu-timelapse/internal/probe"
)

// probeFile is written and removed to prove the staging tree is writable,
// which a read-only mount or a wrong container user makes it not.
const (
	probeFile = ".preflight"
	// overlayCheck names the caption in the preflight log; it is the string
	// an operator greps for when a timelapse arrives without one.
	overlayCheck = "overlay"
)

// check is one thing the host either provides or does not.
//
// Fatal separates "this service cannot do its job" from "this run will be
// slightly worse and here is exactly how". Only the first is worth refusing
// to start over: a printer runs for hours and a capture missed because the
// service exited over a font is not recoverable, while a timelapse without a
// caption is a timelapse.
type check struct {
	name   string
	detail string
	ok     bool
	fatal  bool
}

// preflight asks the host, once, for everything the service will need hours
// from now, and says plainly what it found.
func (s *Service) preflight(ctx context.Context) error {
	sup := s.tools.Detect(ctx)
	writable, writeErr := s.stagingWritable()

	free, freeErr := freeBytes(s.store.Root())
	lowSpace := freeErr == nil && free < s.cfg.MinFree

	checks := []check{
		{
			name: "ffmpeg",
			detail: found(sup.FFmpeg, fmt.Sprintf(
				"%q not found; no frame can be grabbed or encoded (set FFMPEG_BIN)", s.cfg.FFmpegBin)),
			ok:    sup.FFmpeg != "",
			fatal: true,
		},
		{
			name: "ffprobe",
			detail: found(sup.FFprobe, fmt.Sprintf(
				"%q not found; the video will carry no dimensions (set FFPROBE_BIN)", s.cfg.FFprobeBin)),
			ok: sup.FFprobe != "",
		},
		{
			name:   "staging",
			detail: s.store.Root(),
			ok:     writable,
			fatal:  true,
		},
	}
	if writeErr != nil {
		checks[2].detail = fmt.Sprintf("%s: %v", s.store.Root(), writeErr)
	}
	if lowSpace {
		checks = append(checks, check{
			name:   "free space",
			detail: fmt.Sprintf("%d MiB free, below the %d MiB floor", free>>mibShift, s.cfg.MinFree>>mibShift),
		})
	}
	checks = append(checks, s.captionChecks(sup)...)
	checks = append(checks, s.printerChecks(ctx, sup)...)

	var fatal []error
	for _, c := range checks {
		switch {
		case c.ok:
			s.log.Info("preflight", "check", c.name, "ok", true, "detail", c.detail)
		case c.fatal:
			fatal = append(fatal, fmt.Errorf("%s: %s", c.name, c.detail))
			s.log.Error("preflight", "check", c.name, "ok", false, "detail", c.detail)
		default:
			s.log.Warn("preflight", "check", c.name, "ok", false, "detail", c.detail)
		}
	}
	if len(fatal) > 0 {
		return fmt.Errorf("preflight: %w", errors.Join(fatal...))
	}
	return nil
}

// found describes a lookup either way round, so a failed check never logs an
// empty detail and leaves the reader guessing.
func found(path, missing string) string {
	if path == "" {
		return missing
	}
	return path
}

// printerChecks asks the printer itself the questions a missing timelapse is
// eventually traced back to: wrong address, wrong access code, LAN Only
// Liveview still off. None of them is fatal — the printer is allowed to be
// off when the service starts, and MQTT reconnects on its own — but each one
// is hours of silence explained at the second it is knowable.
func (s *Service) printerChecks(ctx context.Context, sup camera.Support) []check {
	cam := s.cam
	if sup.FFmpeg == "" {
		// Already reported as fatal; a grab without ffmpeg only repeats it.
		cam = nil
	}
	results := probe.Printer(ctx, s.cfg.Host, s.cfg.AccessCode, cam, "")

	checks := make([]check, 0, len(results))
	for _, r := range results {
		checks = append(checks, check{name: r.Name, detail: r.Detail, ok: r.OK})
	}
	return checks
}

// captionChecks reports on the decoration, and turns off whatever this ffmpeg
// cannot draw. A filter missing from the build would otherwise fail the
// encode itself, taking the footage with it.
func (s *Service) captionChecks(sup camera.Support) []check {
	var checks []check

	if s.cfg.Overlay {
		wanted := []string{camera.FilterDrawtext, camera.FilterSendcmd}
		var missing []string
		for _, f := range wanted {
			if !sup.Has(f) {
				missing = append(missing, f)
			}
		}
		switch {
		case sup.FFmpeg == "":
			// Already reported as fatal; saying it twice helps nobody.
		case len(missing) > 0:
			s.font = ""
			checks = append(checks, check{
				name:   overlayCheck,
				detail: "ffmpeg lacks " + strings.Join(missing, ", ") + "; captions are off",
			})
		case s.font == "":
			checks = append(checks, check{
				name:   overlayCheck,
				detail: "no font to draw with; captions are off",
			})
		default:
			checks = append(checks, check{name: overlayCheck, detail: s.font, ok: true})
		}
	}

	if s.cfg.Tail > 0 && sup.FFmpeg != "" && !sup.Has(camera.FilterTpad) {
		s.cfg.Tail = 0
		checks = append(checks, check{
			name:   "tail hold",
			detail: "ffmpeg lacks tpad; the video will end on the last frame",
		})
	}
	if s.cfg.Intro > 0 && sup.FFmpeg != "" && !sup.Has(camera.FilterConcat) {
		s.cfg.Intro = 0
		checks = append(checks, check{
			name:   "cover intro",
			detail: "ffmpeg lacks concat; the video will open on the first frame",
		})
	}
	return checks
}
