package camera

import (
	"context"
	"os/exec"
	"strings"
)

// Filters this package puts in a filtergraph and that a minimal ffmpeg build
// can genuinely lack: drawtext needs libfreetype, and a build without it
// still runs everything else perfectly.
// nameField is where ffmpeg -filters prints the filter's name: the line is
// "flags name in->out description".
const nameField = 2

const (
	FilterDrawtext = "drawtext"
	FilterSendcmd  = "sendcmd"
	FilterTpad     = "tpad"
	FilterConcat   = "concat"
)

// Support is what this host's ffmpeg can actually do.
//
// Asked once, at startup. The alternative is finding out at the encode, which
// runs a single time, at the end of a print, with every frame already captured
// and no way to take them again.
type Support struct {
	FFmpeg  string
	FFprobe string
	Filters map[string]bool
}

// parseFilters reads `ffmpeg -filters` output, whose lines are
// "flags name in->out description". The name is matched as a whole field: as
// a substring, "concat" turns up inside other filters' descriptions.
func parseFilters(out string) map[string]bool {
	filters := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < nameField {
			continue
		}
		switch name := fields[nameField-1]; name {
		case FilterDrawtext, FilterSendcmd, FilterTpad, FilterConcat:
			filters[name] = true
		}
	}
	return filters
}

// Has reports whether a filter is compiled into this ffmpeg.
func (s Support) Has(filter string) bool { return s.Filters[filter] }

// Detect probes the host for the binaries and filters this package uses.
func (t Tools) Detect(ctx context.Context) Support {
	sup := Support{Filters: map[string]bool{}}
	sup.FFmpeg, _ = exec.LookPath(t.FFmpeg)
	sup.FFprobe, _ = exec.LookPath(t.FFprobe)
	if sup.FFmpeg == "" {
		return sup
	}

	out, err := t.ffmpeg(ctx, "-hide_banner", "-filters").Output()
	if err != nil {
		return sup
	}
	sup.Filters = parseFilters(string(out))
	return sup
}
