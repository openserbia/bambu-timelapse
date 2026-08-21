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
	// Layer 3 was skipped, a grab was still in flight, so frame 3 is layer
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

	// No plate preview was fetched, so nothing is held at the head: 10s of
	// footage plus the 5s finished shot at the end.
	if got := svc.duration(sess); got != 15 {
		t.Fatalf("duration = %ds, want 15 (10 footage + 5 tail)", got)
	}
}

func TestChangedLayer(t *testing.T) {
	cases := []struct {
		layer, last int
		want        bool
	}{
		{layer: 1, last: -1, want: true},  // the first layer of a fresh session
		{layer: 12, last: 11, want: true}, // the ordinary case
		{layer: 11, last: 11, want: false},
		// A lower number belongs to a different print. Requiring an increase
		// here is what let a 60-layer job's stale count silence a 13-layer one.
		{layer: 3, last: 60, want: true},
		{layer: 0, last: 60, want: false}, // no layer reported yet
	}
	for _, c := range cases {
		if got := changedLayer(c.layer, c.last); got != c.want {
			t.Errorf("changedLayer(%d, %d) = %v, want %v", c.layer, c.last, got, c.want)
		}
	}
}

func TestRecordingIsKeptInsteadOfPosted(t *testing.T) {
	svc := testService(t)
	out := t.TempDir()
	svc.cfg.APIURL = "" // what puts the service in local mode
	svc.cfg.OutputDir = out

	sess, _, err := svc.store.Create("task-1", "Bracket", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	sess.Frames = 42
	video := filepath.Join(sess.Dir(), "video.mp4")
	cover := filepath.Join(sess.Dir(), "cover.jpg")
	for _, f := range []string{video, cover} {
		if err := os.WriteFile(f, []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	svc.deliverLocally(sess, video, cover, "✅ Bracket", "p2s-01_Bracket.mp4")

	for _, want := range []string{
		"p2s-01_Bracket.mp4",
		"p2s-01_Bracket-cover.jpg",
		// The caption the post would have carried, next to the video: reading
		// it is how you check it without a chat on the other end.
		"p2s-01_Bracket.txt",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("%s not written: %v", want, err)
		}
	}
	if _, err := os.Stat(sess.Dir()); !os.IsNotExist(err) {
		t.Errorf("staging kept after a local delivery: %v", err)
	}
}

func TestOnceStopsAfterOneRecording(t *testing.T) {
	svc := testService(t)
	svc.cfg.APIURL = ""
	svc.cfg.OutputDir = t.TempDir()
	svc.cfg.Once = true
	stopped := false
	svc.stop = func() { stopped = true }

	sess, _, err := svc.store.Create("task-1", "Bracket", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(sess.Dir(), "video.mp4")
	if err := os.WriteFile(video, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc.deliverLocally(sess, video, "", "caption", "out.mp4")

	if !stopped {
		t.Fatal("-once did not end the run after the print it was started for")
	}
}

func TestUnwritableOutputParksRatherThanLoses(t *testing.T) {
	// The video is the print. If it cannot be saved where the operator asked,
	// it stays in failed/ for them to fetch, not deleted.
	svc := testService(t)
	svc.cfg.APIURL = ""
	svc.cfg.OutputDir = filepath.Join(t.TempDir(), "file", "under", "a", "file")
	if err := os.WriteFile(filepath.Dir(filepath.Dir(filepath.Dir(svc.cfg.OutputDir))), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, _, err := svc.store.Create("task-1", "Bracket", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(sess.Dir(), "video.mp4")
	if err := os.WriteFile(video, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc.deliverLocally(sess, video, "", "caption", "out.mp4")

	parked, err := os.ReadDir(svc.store.FailedDir())
	if err != nil || len(parked) != 1 {
		t.Fatalf("job not parked: %v %v", parked, err)
	}
}

// captureLogs gives a service a logger whose output the test can read.
func captureLogs(t *testing.T) (*Service, *strings.Builder) {
	t.Helper()
	svc := testService(t)
	var out strings.Builder
	svc.log = slog.New(slog.NewTextHandler(&out, nil))
	return svc, &out
}

func TestIdleIsReportedOncePerState(t *testing.T) {
	// The printer reports every few seconds. Saying it every time buries the
	// log; never saying it makes a service that is waiting look like one that
	// has stopped listening.
	svc, out := captureLogs(t)

	svc.noteIdle("IDLE")
	svc.noteIdle("IDLE")
	svc.noteIdle("IDLE")

	if got := strings.Count(out.String(), "waiting for a job"); got != 1 {
		t.Fatalf("logged the idle note %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(out.String(), "state=IDLE") {
		t.Fatalf("the state is not in the line:\n%s", out)
	}
}

func TestAChangeOfStateIsReportedImmediately(t *testing.T) {
	svc, out := captureLogs(t)

	svc.noteIdle("IDLE")
	svc.noteIdle("FINISH")

	if got := strings.Count(out.String(), "waiting for a job"); got != 2 {
		t.Fatalf("a new state was not reported:\n%s", out)
	}
}

func TestTheReminderRepeatsAfterTheInterval(t *testing.T) {
	svc, out := captureLogs(t)

	svc.noteIdle("IDLE")
	svc.idleLogged = time.Now().Add(-idleReminder - time.Second)
	svc.noteIdle("IDLE")

	if got := strings.Count(out.String(), "waiting for a job"); got != 2 {
		t.Fatalf("the reminder did not repeat:\n%s", out)
	}
}
