package service

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openserbia/bambu-timelapse/internal/config"
	"github.com/openserbia/bambu-timelapse/internal/session"
)

func testService(t *testing.T) *Service {
	t.Helper()
	cfg := &config.Config{
		PrinterName: "p2s-01",
		StagingDir:  t.TempDir(),
		FPS:         20,
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestCaptionMarksPartialCaptures(t *testing.T) {
	svc := testService(t)
	sess := &session.Session{
		JobName: "Bracket", LastLayer: 400, TotalLayers: 499, Partial: true,
	}
	caption := svc.caption(sess, "FINISH", 90*time.Minute)

	// Presenting 1h30m as the print duration when capture began at layer 250
	// is a claim the video itself contradicts.
	if !strings.Contains(caption, "capture started mid-print") {
		t.Fatalf("partial capture not disclosed:\n%s", caption)
	}
}

func TestCaptionEscapesJobNames(t *testing.T) {
	svc := testService(t)
	sess := &session.Session{JobName: `Bracket <v3> & "spare"`}
	caption := svc.caption(sess, "FINISH", time.Minute)

	if strings.Contains(caption, "<v3>") {
		t.Fatalf("job name was not escaped into the HTML caption:\n%s", caption)
	}
	if !strings.Contains(caption, "&lt;v3&gt;") {
		t.Fatalf("expected escaped angle brackets:\n%s", caption)
	}
}

func TestCaptionReflectsOutcome(t *testing.T) {
	svc := testService(t)
	sess := &session.Session{JobName: "Bracket"}

	if got := svc.caption(sess, "FINISH", time.Hour); !strings.HasPrefix(got, "✅") {
		t.Fatalf("finished print caption starts %q", got[:8])
	}
	if got := svc.caption(sess, "FAILED", time.Hour); !strings.HasPrefix(got, "❌") {
		t.Fatalf("failed print caption starts %q", got[:8])
	}
}

func TestCaptionOmitsUnknowns(t *testing.T) {
	svc := testService(t)
	// A print with no AMS data and no temperature samples must not render
	// "Filament: " or "Nozzle 0°C".
	caption := svc.caption(&session.Session{JobName: "x"}, "FINISH", time.Minute)
	for _, unwanted := range []string{"Filament:", "Nozzle", "pause"} {
		if strings.Contains(caption, unwanted) {
			t.Fatalf("caption should omit %q when unknown:\n%s", unwanted, caption)
		}
	}
}

func TestFilenameIsSanitisedAndStamped(t *testing.T) {
	svc := testService(t)
	started := time.Date(2026, 8, 20, 14, 32, 0, 0, time.UTC)
	sess := &session.Session{
		JobName:   "AMS1 Complete Set (0.2mm layer)/../etc",
		StartedAt: started,
	}
	got := svc.filename(sess)

	if strings.ContainsAny(got, "/ ()") {
		t.Fatalf("filename not sanitised: %q", got)
	}
	if !strings.HasPrefix(got, "p2s-01_") || !strings.HasSuffix(got, "_2026-08-20T14-32.mp4") {
		t.Fatalf("filename = %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:             "0m",
		90 * time.Second:             "1m",
		42 * time.Minute:             "42m",
		6*time.Hour + 42*time.Minute: "6h42m",
		25*time.Hour + 5*time.Minute: "25h05m",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{ //nolint:gosec // G101: these are job titles, not credentials
		"AMS1 Complete Set": "AMS1_Complete_Set",
		"../../etc/passwd":  "_.._etc_passwd", // leading dots stripped by design
		"":                  "print",
		"...":               "print",
		"Držač":             "Dr_a_",
	}
	for in, want := range cases {
		if got := clean(in, 40); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverlayCountsCapturedLayersNotFrames(t *testing.T) {
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.font = "/font.ttf"
	// Layer 3 was skipped — a grab was still in flight — so frame 3 is layer
	// 4. Numbering the caption off the frame index would say layer 3 while the
	// footage shows the layer after it.
	sess := &session.Session{
		JobName: "Bracket", TotalLayers: 500, Frames: 3, Layers: []int{1, 2, 4},
	}
	o := svc.overlay(sess)
	if o == nil {
		t.Fatal("no overlay built")
	}
	if o.Title != "p2s-01 · Bracket" {
		t.Fatalf("title = %q", o.Title)
	}
	want := []string{"Layer 1/500", "Layer 2/500", "Layer 4/500"}
	for i, line := range want {
		if o.Lines[i] != line {
			t.Errorf("line %d = %q, want %q", i, o.Lines[i], line)
		}
	}
}

func TestOverlayDropsCounterWhenLayersAreUnknown(t *testing.T) {
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.font = "/font.ttf"
	// A state file written before the layer list existed: frames captured,
	// nothing that says which layer any of them is.
	sess := &session.Session{JobName: "Bracket", Frames: 12}

	o := svc.overlay(sess)
	if o == nil {
		t.Fatal("no overlay built")
	}
	if len(o.Lines) != 0 {
		t.Fatalf("counter invented from %d frames and no layers: %v", sess.Frames, o.Lines)
	}
}

func TestOverlayOffProducesNone(t *testing.T) {
	svc := testService(t)
	svc.cfg.Overlay = false
	svc.font = "/font.ttf"
	if o := svc.overlay(&session.Session{Frames: 3, Layers: []int{1, 2, 3}}); o != nil {
		t.Fatalf("overlay built while disabled: %+v", o)
	}
}

func TestOverlayNeedsAFont(t *testing.T) {
	// No font resolved at startup: the print is still recorded and posted,
	// just without a caption drawn over it.
	svc := testService(t)
	svc.cfg.Overlay = true
	svc.font = ""
	if o := svc.overlay(&session.Session{Frames: 3, Layers: []int{1, 2, 3}}); o != nil {
		t.Fatalf("overlay built with no font: %+v", o)
	}
}

func TestBundledFontIsUsedWhenNoneIsConfigured(t *testing.T) {
	svc := testService(t)
	font := svc.resolveFont()
	if font == "" {
		t.Fatal("no font resolved; the binary carries one")
	}
	if _, err := os.Stat(font); err != nil {
		t.Fatalf("resolved font is not on disk: %v", err)
	}
}

func TestUnreadableFontFallsBackToTheBundledOne(t *testing.T) {
	svc := testService(t)
	svc.cfg.OverlayFont = filepath.Join(t.TempDir(), "missing.ttf")

	font := svc.resolveFont()
	if font == "" || font == svc.cfg.OverlayFont {
		t.Fatalf("resolveFont() = %q, want the bundled font", font)
	}
}

func TestDurationCountsBothHelds(t *testing.T) {
	svc := testService(t)
	svc.cfg.Intro = 5 * time.Second
	svc.cfg.Tail = 5 * time.Second
	sess := &session.Session{Frames: 200} // 10s at 20fps

	if got := svc.duration(sess, "cover.jpg"); got != 20 {
		t.Fatalf("duration = %ds, want 20 (5 intro + 10 footage + 5 tail)", got)
	}
	// No cover means no intro was concatenated, so it must not be counted.
	if got := svc.duration(sess, ""); got != 15 {
		t.Fatalf("duration without a cover = %ds, want 15", got)
	}
}
