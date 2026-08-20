package service

import (
	"io"
	"log/slog"
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
