package service

import (
	"strings"
	"testing"
	"time"

	"github.com/openserbia/bambu-timelapse/internal/camera"
)

func fullSupport() camera.Support {
	return camera.Support{
		FFmpeg:  "/usr/bin/ffmpeg",
		FFprobe: "/usr/bin/ffprobe",
		Filters: map[string]bool{
			camera.FilterDrawtext: true,
			camera.FilterSendcmd:  true,
			camera.FilterTpad:     true,
			camera.FilterConcat:   true,
		},
	}
}

func TestPreflightPassesOnACompleteHost(t *testing.T) {
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.font = "/staging/overlay-font.ttf"

	for _, c := range svc.captionChecks(fullSupport()) {
		if !c.ok {
			t.Fatalf("check %q failed on a complete host: %s", c.name, c.detail)
		}
	}
	if svc.font == "" {
		t.Fatal("captions turned off on a host that can draw them")
	}
}

func TestMissingDrawtextTurnsCaptionsOffRatherThanFailing(t *testing.T) {
	// An ffmpeg built without libfreetype runs everything else perfectly. The
	// encode would fail on the filtergraph alone, taking hours of footage
	// with it, so the caption is dropped at startup instead.
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.font = "/staging/overlay-font.ttf"

	sup := fullSupport()
	delete(sup.Filters, camera.FilterDrawtext)

	checks := svc.captionChecks(sup)
	if svc.font != "" {
		t.Fatal("captions left on with no drawtext to draw them")
	}
	if !strings.Contains(detailOf(t, checks, "overlay"), "drawtext") {
		t.Fatalf("the missing filter is not named: %+v", checks)
	}
}

func TestMissingPadFiltersDropTheHeldEnds(t *testing.T) {
	svc := testService(t)
	svc.cfg.Overlay = false
	svc.cfg.Intro = 5 * time.Second
	svc.cfg.Tail = 5 * time.Second

	sup := fullSupport()
	delete(sup.Filters, camera.FilterTpad)
	delete(sup.Filters, camera.FilterConcat)

	checks := svc.captionChecks(sup)
	if svc.cfg.Tail != 0 || svc.cfg.Intro != 0 {
		t.Fatalf("holds survived a build that cannot pad: intro=%v tail=%v",
			svc.cfg.Intro, svc.cfg.Tail)
	}
	if len(checks) != 2 {
		t.Fatalf("expected a check per dropped hold, got %+v", checks)
	}
}

func TestFilterChecksAreSilentWithoutFFmpeg(t *testing.T) {
	// ffmpeg's absence is already fatal and already reported; repeating it
	// once per filter buries the line that matters.
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.cfg.Intro = 5 * time.Second
	svc.cfg.Tail = 5 * time.Second

	if checks := svc.captionChecks(camera.Support{Filters: map[string]bool{}}); len(checks) != 0 {
		t.Fatalf("filters reported with no ffmpeg to run them: %+v", checks)
	}
}

func TestPreflightRefusesToStartWithoutFFmpeg(t *testing.T) {
	svc := testService(t)
	err := svc.preflight(t.Context())
	if err == nil {
		t.Skip("this host has ffmpeg; nothing to refuse")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("preflight failed for the wrong reason: %v", err)
	}
}

func detailOf(t *testing.T, checks []check, name string) string {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c.detail
		}
	}
	t.Fatalf("no %q check in %+v", name, checks)
	return ""
}
