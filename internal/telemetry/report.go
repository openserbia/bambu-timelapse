// Package telemetry decodes the printer's MQTT report topic.
//
// Reports are DELTAS, not snapshots: a message may carry nothing but
// {"layer_num": 412}. Assigning rather than merging would wipe gcode_state on
// every such message and make each one look like a state transition, so the
// merged view is kept as a map and typed accessors read from it.
package telemetry

import (
	"encoding/json"
	"strconv"
	"strings"
)

// The printer's gcode_state values. Named so the lifecycle switches read as
// states rather than as string literals repeated across packages.
const (
	StateRunning = "RUNNING"
	StatePause   = "PAUSE"
	StateFinish  = "FINISH"
	StateFailed  = "FAILED"
	StateIdle    = "IDLE"
)

const (
	// reportFields is a generous starting size for the merged view; the P2S
	// reports ~98 keys in its "print" section.
	reportFields = 128
	// colourHexLen is how much of an AMS tray_color is the RGB triplet; the
	// remaining two characters are alpha.
	colourHexLen = 6
	// maxFilaments bounds the caption's filament list.
	maxFilaments = 4
)

// State is the merged view of everything the printer has reported.
type State struct {
	fields map[string]any
}

// NewState returns an empty merged view.
func NewState() *State {
	return &State{fields: make(map[string]any, reportFields)}
}

// Merge folds one report's "print" section into the view. It reports whether
// the payload was a report at all.
func (s *State) Merge(payload []byte) bool {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return false
	}
	raw, ok := msg["print"]
	if !ok {
		return false
	}
	var section map[string]any
	if err := json.Unmarshal(raw, &section); err != nil {
		return false
	}
	for k, v := range section {
		s.fields[k] = v
	}
	return true
}

// Has reports whether a key was ever seen.
func (s *State) Has(key string) bool {
	_, ok := s.fields[key]
	return ok
}

// String reads a string field, tolerating a numeric one.
func (s *State) String(key string) string {
	switch v := s.fields[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// Int reads a numeric field, tolerating a stringified one.
func (s *State) Int(key string) int {
	switch v := s.fields[key].(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// Float reads a numeric field.
func (s *State) Float(key string) float64 {
	switch v := s.fields[key].(type) {
	case float64:
		return v
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// GcodeState is the print lifecycle state: RUNNING, PAUSE, FINISH, FAILED,
// IDLE, PREPARE, SLICING.
func (s *State) GcodeState() string { return s.String("gcode_state") }

// Layer is the layer currently being printed.
func (s *State) Layer() int { return s.Int("layer_num") }

// TotalLayers is the job's layer count.
func (s *State) TotalLayers() int { return s.Int("total_layer_num") }

// JobName is the human-readable job title.
func (s *State) JobName() string { return s.String("subtask_name") }

// TaskID identifies the job, and is what makes a resume after a restart
// possible at all.
//
// Cloud-dispatched prints populate task_id and leave lan_task_id as "0";
// LAN-dispatched prints do the reverse. Trying only one of them silently
// breaks resume for half of all prints — and in the direction that is hardest
// to notice, since the service still records perfectly well, it just starts a
// fresh session on every restart.
func (s *State) TaskID() string {
	for _, key := range []string{"task_id", "subtask_id", "lan_task_id", "job_id"} {
		if v := s.String(key); v != "" && v != "0" {
			return v
		}
	}
	return ""
}

// Nozzle is the current nozzle temperature.
func (s *State) Nozzle() float64 { return s.Float("nozzle_temper") }

// Bed is the current bed temperature.
func (s *State) Bed() float64 { return s.Float("bed_temper") }

// Progress is the reported completion percentage.
func (s *State) Progress() int { return s.Int("mc_percent") }

// Filament summarises the loaded AMS trays as "PLA #0086D6, PETG #161616",
// deduplicated: four slots of the same material should read as one entry.
func (s *State) Filament() string {
	ams, ok := s.fields["ams"].(map[string]any)
	if !ok {
		return ""
	}
	units, ok := ams["ams"].([]any)
	if !ok {
		return ""
	}
	var out []string
	seen := make(map[string]bool)
	for _, u := range units {
		unit, ok := u.(map[string]any)
		if !ok {
			continue
		}
		trays, ok := unit["tray"].([]any)
		if !ok {
			continue
		}
		for _, t := range trays {
			tray, ok := t.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := tray["tray_type"].(string)
			if kind == "" {
				continue
			}
			colour, _ := tray["tray_color"].(string)
			if len(colour) > colourHexLen {
				colour = colour[:colourHexLen]
			}
			entry := kind
			if colour != "" {
				entry += " #" + colour
			}
			if !seen[entry] {
				seen[entry] = true
				out = append(out, entry)
			}
		}
	}
	if len(out) > maxFilaments {
		out = out[:maxFilaments]
	}
	return strings.Join(out, ", ")
}
