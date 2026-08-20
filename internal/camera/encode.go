package camera

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// framePattern is how session names the stills it captures.
	framePattern = "frame-%05d.jpg"
	// firstFrame is probed for the frame size when no crop pins it down.
	firstFrame = "frame-00001.jpg"

	// commandsFile carries one drawtext command per frame. It lives in the
	// job directory rather than a temp file so a parked job can be re-encoded
	// by hand with exactly the overlay it would have had.
	commandsFile = "overlay.cmds"

	// Filter instance names. sendcmd addresses the counter by name, so the
	// static title can keep its own styling instead of being re-sent whole
	// with every layer.
	titleFilter   = "drawtext@title"
	counterFilter = "drawtext@counter"

	// Overlay geometry, all relative to frame height so a cropped video and a
	// full-frame one are captioned at the same visual size.
	fontDivisor     = 26
	boxBorder       = 8
	titleBaseline   = "h-2*th-h*0.055"
	counterBaseline = "h-th-h*0.03"
	overlayLeft     = "w*0.02"

	// commandLead pulls each command a quarter of a frame earlier than the
	// frame it belongs to. sendcmd fires on the first pts at or past its
	// timestamp, and a frame's pts is a rational that can land a hair below
	// the decimal written here — without the lead that rounding shows the
	// previous layer's number for one frame.
	commandLead = 0.25
	// commandsPerm keeps the command file owner-only, like the rest of staging.
	commandsPerm = 0o600
	// timeDigits keeps the command timestamps readable and exact enough at
	// any sane frame rate.
	timeDigits = 3
)

// Overlay is the caption burned into the footage.
//
// Lines is per FRAME, not per layer: frames are numbered sequentially and a
// layer whose grab was skipped leaves no frame, so anything derived from the
// frame index alone drifts further from the truth with every drop. Nil Lines
// draws the title only, which is what a session recorded before the overlay
// existed gets on resume.
type Overlay struct {
	FontFile string
	Title    string
	Lines    []string
}

// Still is an image held for a fixed time at one end of the video.
type Still struct {
	Path string
	Hold time.Duration
}

func (s Still) shown() bool { return s.Path != "" && s.Hold > 0 }

// EncodeOptions is everything the final encode needs beyond the frames.
type EncodeOptions struct {
	FPS  int
	Crop string
	// Intro opens the video — what the print was meant to be. Outro closes it
	// with what it turned out to be, which is the frame worth ending on: the
	// last captured layer still has the toolhead sitting in it.
	Intro Still
	Outro Still
	// TailHold pads the end by cloning the last frame, and is the fallback for
	// when there is no finished shot to end on.
	TailHold time.Duration
	Overlay  *Overlay
}

// Encode muxes the numbered frames in dir into an H.264 mp4, optionally
// cropping, captioning, and padding both ends. Cropping happens here rather
// than at capture time so the frames on disk stay whole: a crop can then be
// retuned and the video re-encoded without reprinting anything.
func (t Tools) Encode(ctx context.Context, dir, out string, opts EncodeOptions) error {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.Itoa(opts.FPS),
		"-i", filepath.Join(dir, framePattern),
	}

	stills := []Still{}
	if opts.Intro.shown() {
		stills = append(stills, opts.Intro)
	}
	if opts.Outro.shown() {
		stills = append(stills, opts.Outro)
	}
	var width, height int
	if len(stills) > 0 {
		// An unprobeable frame costs the held ends, not the video: parking a
		// finished print over a piece of decoration would be the worse trade.
		if width, height = t.targetSize(ctx, dir, opts.Crop); width == 0 {
			stills = nil
		}
	}
	for _, still := range stills {
		args = append(
			args,
			"-loop", "1",
			"-framerate", strconv.Itoa(opts.FPS),
			"-t", seconds(still.Hold),
			"-i", still.Path,
		)
	}

	graph, label, err := buildGraph(dir, opts, len(stills) > 0, width, height)
	if err != nil {
		return err
	}

	args = append(
		args,
		"-filter_complex", graph, "-map", label,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		// yuv420p for player compatibility. +faststart moves the moov atom to
		// the front, which is what lets Telegram render an inline player with
		// a poster frame instead of a grey file row.
		"-pix_fmt", "yuv420p", "-movflags", "+faststart",
		out,
	)

	// G204: dir, out and the cover are paths this service created inside its
	// own staging tree; the crop and the font are validated at startup and the
	// caption text is sanitised by overlayText.
	//nolint:gosec // see above
	cmd := t.ffmpeg(ctx, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("encode: %w: %s", err, truncate(string(combined), encodeErrBytes))
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		return errors.New("encode produced no file")
	}
	return nil
}

// buildGraph assembles the filtergraph and the output label to map.
//
// One graph for every combination rather than a -vf fast path: the ordering
// is the part that has to be right — crop before the caption so the caption
// is not cropped away, the caption before the tail so the held frame keeps
// it — and a second code path would be a second place to get that wrong.
func buildGraph(dir string, opts EncodeOptions, stills bool, width, height int) (graph, label string, err error) {
	var main []string
	if opts.Crop != "" {
		main = append(main, "crop="+opts.Crop)
	}
	if opts.Overlay != nil {
		filters, err := overlayFilters(dir, opts.Overlay, opts.FPS)
		if err != nil {
			return "", "", err
		}
		main = append(main, filters...)
	}
	// Cloning the last frame is what ends the video when there is no finished
	// shot to end on; with one, the still says the same thing and says it
	// about the print rather than about its last layer.
	if opts.TailHold > 0 && !opts.Outro.shown() {
		main = append(main, "tpad=stop_mode=clone:stop_duration="+seconds(opts.TailHold))
	}
	// setsar keeps concat from refusing two streams that differ only in a
	// sample aspect ratio neither of them meaningfully has.
	main = append(main, "format=yuv420p", "setsar=1")

	graph = "[0:v]" + strings.Join(main, ",") + "[main]"
	if !stills {
		return graph, "[main]", nil
	}

	// Input 0 is the frames; the stills follow in the order they were added,
	// which is the order they are concatenated in.
	var (
		segments []string
		input    = 1
	)
	if opts.Intro.shown() {
		graph += stillChain(input, "intro", width, height, opts.FPS)
		segments = append(segments, "[intro]")
		input++
	}
	segments = append(segments, "[main]")
	if opts.Outro.shown() {
		graph += stillChain(input, "outro", width, height, opts.FPS)
		segments = append(segments, "[outro]")
	}
	graph += fmt.Sprintf(";%sconcat=n=%d:v=1:a=0[out]",
		strings.Join(segments, ""), len(segments))
	return graph, "[out]", nil
}

// stillChain conforms one held image to the footage it sits next to. concat
// refuses inputs that disagree on size, pixel format, aspect or frame rate,
// and a slicer's preview render agrees with the camera on none of them.
func stillChain(input int, label string, width, height, fps int) string {
	return fmt.Sprintf(
		";[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,"+
			"format=yuv420p,setsar=1,fps=%d[%s]",
		input, width, height, width, height, fps, label)
}

// overlayFilters writes the per-frame command file and returns the drawtext
// chain that reads it.
func overlayFilters(dir string, o *Overlay, fps int) ([]string, error) {
	style := fmt.Sprintf(
		"fontfile=%s:x=%s:fontsize=h/%d:fontcolor=white:box=1:boxcolor=black@0.45:boxborderw=%d",
		o.FontFile, overlayLeft, fontDivisor, boxBorder,
	)

	title := fmt.Sprintf("%s=text='%s':y=%s:%s",
		titleFilter, overlayText(o.Title), titleBaseline, style)
	if len(o.Lines) == 0 {
		return []string{title}, nil
	}

	path := filepath.Join(dir, commandsFile)
	if err := writeCommands(path, o.Lines, fps); err != nil {
		return nil, err
	}
	counter := fmt.Sprintf("%s=text='%s':y=%s:%s",
		counterFilter, overlayText(o.Lines[0]), counterBaseline, style)
	// sendcmd sits ahead of both drawtexts because it addresses them by name;
	// a filter cannot be commanded before it exists in the graph.
	return []string{"sendcmd=f=" + path, title, counter}, nil
}

// writeCommands emits one `text` command per frame. Setting the option
// directly rather than reinit-ing the filter is what lets the styling be
// declared once in the graph instead of repeated on all several hundred lines.
func writeCommands(path string, lines []string, fps int) error {
	var b strings.Builder
	for i, line := range lines {
		at := (float64(i) - commandLead) / float64(fps)
		if at < 0 {
			at = 0
		}
		b.WriteString(strconv.FormatFloat(at, 'f', timeDigits, 64))
		b.WriteString(" " + counterFilter + " text '")
		b.WriteString(overlayText(line))
		b.WriteString("';\n")
	}
	return os.WriteFile(path, []byte(b.String()), commandsPerm)
}

// overlayText strips what would otherwise be read as syntax.
//
// A job name arrives from the printer and can hold anything the slicer let
// someone type. Escaping it correctly would mean escaping for three nested
// parsers at once — the filtergraph, the filter's own option list, and
// drawtext's %{} expansion — so the characters that mean something to any of
// them are dropped instead. A caption is decoration; a filtergraph that
// fails to parse loses the whole video.
func overlayText(s string) string {
	const dropped = `\':%,;[]=`
	cleaned := strings.Map(func(r rune) rune {
		if r < ' ' || strings.ContainsRune(dropped, r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(cleaned)
}

// targetSize is the size the intro still must be scaled to: the crop when one
// is configured, and otherwise whatever the frames themselves are.
func (t Tools) targetSize(ctx context.Context, dir, crop string) (width, height int) {
	if crop != "" {
		parts := strings.Split(crop, ":")
		if len(parts) < dimensionParts {
			return 0, 0
		}
		w, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0
		}
		h, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0
		}
		return w, h
	}
	return t.Dimensions(ctx, filepath.Join(dir, firstFrame))
}

// seconds formats a duration the way ffmpeg's filters want it.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}
