package camera

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testOverlay() *Overlay {
	return &Overlay{
		FontFile: "/usr/share/fonts/dejavu/DejaVuSans.ttf",
		Title:    "p2s-01 · Bracket",
		Lines:    []string{"Layer 1/500", "Layer 3/500", "Layer 4/500"},
	}
}

func TestGraphOrdersCropCaptionTail(t *testing.T) {
	// The ordering is the whole point: a caption drawn before the crop can be
	// cropped away, and a tail padded before the caption holds an uncaptioned
	// frame for five seconds.
	graph, label, err := buildGraph(t.TempDir(), EncodeOptions{
		FPS: 20, Crop: "1920:820:0:260", TailHold: 5 * time.Second, Overlay: testOverlay(),
	}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if label != "[main]" {
		t.Fatalf("label = %q with no intro", label)
	}
	order := []string{"crop=", "sendcmd=", titleFilter, counterFilter, "tpad="}
	at := -1
	for _, want := range order {
		i := strings.Index(graph, want)
		if i < 0 {
			t.Fatalf("%q missing from graph:\n%s", want, graph)
		}
		if i < at {
			t.Fatalf("%q comes out of order in:\n%s", want, graph)
		}
		at = i
	}
}

func TestGraphConcatsIntroWhenCoverIsSized(t *testing.T) {
	graph, label, err := buildGraph(t.TempDir(), EncodeOptions{
		FPS:   20,
		Intro: Still{Path: "preview.png", Hold: 2 * time.Second},
		Outro: Still{Path: "cover.jpg", Hold: 2 * time.Second},
	}, true, 1920, 820)
	if err != nil {
		t.Fatal(err)
	}
	if label != "[out]" {
		t.Fatalf("label = %q with an intro", label)
	}
	for _, want := range []string{
		"scale=1920:820", "[intro]", "[outro]",
		// The order is the story: what it was meant to be, the footage, what
		// it became.
		"[intro][main][outro]concat=n=3:v=1:a=0[out]",
	} {
		if !strings.Contains(graph, want) {
			t.Fatalf("%q missing from graph:\n%s", want, graph)
		}
	}
}

func TestGraphWithoutOverlayHasNoDrawtext(t *testing.T) {
	graph, _, err := buildGraph(t.TempDir(), EncodeOptions{FPS: 20}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(graph, "drawtext") || strings.Contains(graph, "sendcmd") {
		t.Fatalf("captioning leaked into an overlay-free encode:\n%s", graph)
	}
}

func TestOverlayWithoutLinesDrawsTitleOnly(t *testing.T) {
	// A session resumed from a state file that predates the layer list has no
	// honest per-frame mapping; it must not fall back to a command file that
	// says nothing.
	dir := t.TempDir()
	o := testOverlay()
	o.Lines = nil
	graph, _, err := buildGraph(dir, EncodeOptions{FPS: 20, Overlay: o}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(graph, counterFilter) || strings.Contains(graph, "sendcmd") {
		t.Fatalf("counter drawn without per-frame layers:\n%s", graph)
	}
	if _, err := os.Stat(filepath.Join(dir, commandsFile)); !os.IsNotExist(err) {
		t.Fatalf("command file written with no lines to command: %v", err)
	}
}

func TestOverlayTextDropsFiltergraphSyntax(t *testing.T) {
	// A job name comes off the printer and is parsed by the filtergraph, the
	// filter's option list and drawtext's own %{} expansion in turn. None of
	// its characters may survive into any of them.
	cases := map[string]string{
		"Bracket:v3":        "Bracket v3",
		`it's [done]`:       "it s  done",
		"100%{eof}":         "100 {eof}",
		"a,b;c=d":           "a b c d",
		"  Držač · v2  ":    "Držač · v2",
		"line\nbreak":       "line break",
		`C:\prints\bracket`: "C  prints bracket",
	}
	for in, want := range cases {
		if got := overlayText(in); got != want {
			t.Errorf("overlayText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteCommandsTimesEveryFrame(t *testing.T) {
	dir := t.TempDir()
	lines := []string{"Layer 1/500", "Layer 3/500", "Layer 4/500"}
	if err := writeCommands(filepath.Join(dir, commandsFile), lines, 20); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, commandsFile)) //nolint:gosec // G304: the path is this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(got) != len(lines) {
		t.Fatalf("%d commands for %d frames:\n%s", len(got), len(lines), body)
	}
	// Each command leads its frame slightly; landing exactly on the frame's
	// pts risks firing one frame late, which would caption the frame with the
	// previous layer's number.
	want := []string{
		"0.000 " + counterFilter + " text 'Layer 1/500';",
		"0.037 " + counterFilter + " text 'Layer 3/500';",
		"0.087 " + counterFilter + " text 'Layer 4/500';",
	}
	for i, line := range want {
		if got[i] != line {
			t.Errorf("command %d = %q, want %q", i, got[i], line)
		}
	}
}

// TestEncodeProducesPlayableVideo runs the real ffmpeg when one is on PATH.
// CI's toolchain deliberately does not ship it, though the runtime image
// does, so this skips rather than fails there.
func TestEncodeProducesPlayableVideo(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH")
	}
	dir := t.TempDir()
	const frames = 40
	for i := 1; i <= frames; i++ {
		writeFrame(t, filepath.Join(dir, "frame-"+pad(i)+".jpg"))
	}
	cover := filepath.Join(dir, "cover.jpg")
	writeFrame(t, cover)

	// The bundled font, not whatever this host has installed: it is the one
	// the service actually draws with.
	font, err := BundledFont(dir)
	if err != nil {
		t.Fatal(err)
	}

	lines := make([]string, frames)
	for i := range lines {
		lines[i] = "Layer " + strconv.Itoa(i*2+1) + "/500"
	}

	out := filepath.Join(dir, "out.mp4")
	if err := NewTools("", "").Encode(t.Context(), dir, out, EncodeOptions{
		FPS:   10,
		Crop:  "320:160:0:20",
		Intro: Still{Path: cover, Hold: 2 * time.Second},
		Outro: Still{Path: cover, Hold: 3 * time.Second},
		Overlay: &Overlay{
			FontFile: font,
			Title:    "p2s-01 · Bracket: it's <fine>",
			Lines:    lines,
		},
	}); err != nil {
		t.Fatalf("encode with %s: %v", ffmpegBin, err)
	}

	// 2s of cover + 4s of footage at 10fps + 3s of held final frame.
	//nolint:gosec // G204: probe is whatever ffprobe LookPath just resolved
	got, err := exec.CommandContext(t.Context(), probe, "-v", "error", "-show_entries",
		"format=duration", "-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(got)), 64)
	if err != nil {
		t.Fatal(err)
	}
	if secs < 8.9 || secs > 9.2 {
		t.Fatalf("duration = %vs, want ~9s (2 intro + 4 footage + 3 tail)", secs)
	}
	if w, h := NewTools("", "").Dimensions(t.Context(), out); w != 320 || h != 160 {
		t.Fatalf("dimensions = %dx%d, want the cropped 320x160", w, h)
	}
}

func writeFrame(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for x := range 320 {
		for y := range 180 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pad(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 5-len(s)) + s
}

func TestBundledFontIsWrittenOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	path, err := BundledFont(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Size() == 0 {
		t.Fatal("bundled font is empty")
	}
	again, err := BundledFont(dir)
	if err != nil || again != path {
		t.Fatalf("BundledFont again = %q, %v", again, err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatal("font rewritten on a second call; it should be reused")
	}
}

func TestParseFiltersMatchesWholeNames(t *testing.T) {
	// The name is a field, not a substring: "concat" appears in other
	// filters' descriptions, and reading it there would report a build as
	// able to do something it cannot.
	out := ` T. drawtext          V->V       Draw text on top of video frames using libfreetype library.
 .. tpad               V->V       Temporarily pad video frames.
 .. interleave         V->V       Temporally interleave video inputs, like concat does.
 ... acrossfade        A->A       Cross fade two input audio streams.
`
	got := parseFilters(out)
	for _, want := range []string{FilterDrawtext, FilterTpad} {
		if !got[want] {
			t.Errorf("%s not detected in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{FilterConcat, FilterSendcmd} {
		if got[unwanted] {
			t.Errorf("%s reported from a description, not a filter name", unwanted)
		}
	}
}

func TestNewToolsFallsBackToPathNames(t *testing.T) {
	if got := NewTools("", ""); got.FFmpeg != "ffmpeg" || got.FFprobe != "ffprobe" {
		t.Fatalf("NewTools(\"\", \"\") = %+v, want the plain names", got)
	}
	// An absolute path is the point of the setting: PATH is an assumption
	// about the caller's environment, not a fact.
	got := NewTools("/opt/ffmpeg/bin/ffmpeg", "/opt/ffmpeg/bin/ffprobe")
	if got.FFmpeg != "/opt/ffmpeg/bin/ffmpeg" || got.FFprobe != "/opt/ffmpeg/bin/ffprobe" {
		t.Fatalf("NewTools() = %+v", got)
	}
}
