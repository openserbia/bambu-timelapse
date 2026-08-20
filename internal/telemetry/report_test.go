package telemetry

import "testing"

func TestMergeKeepsFieldsAcrossDeltas(t *testing.T) {
	s := NewState()
	if !s.Merge([]byte(`{"print":{"gcode_state":"RUNNING","layer_num":10,"total_layer_num":499}}`)) {
		t.Fatal("first report rejected")
	}
	// The printer sends deltas. A message carrying only layer_num must not
	// erase gcode_state — assigning instead of merging would make every such
	// message look like a transition out of RUNNING.
	s.Merge([]byte(`{"print":{"layer_num":11}}`))

	if got := s.GcodeState(); got != "RUNNING" {
		t.Fatalf("gcode_state = %q after a delta, want RUNNING", got)
	}
	if got := s.Layer(); got != 11 {
		t.Fatalf("layer = %d, want 11", got)
	}
	if got := s.TotalLayers(); got != 499 {
		t.Fatalf("total_layer_num = %d, want 499", got)
	}
}

func TestMergeIgnoresNonReports(t *testing.T) {
	s := NewState()
	for _, payload := range []string{`{"liveview":{"x":1}}`, `not json`, `{}`} {
		if s.Merge([]byte(payload)) {
			t.Fatalf("payload %q should not count as a report", payload)
		}
	}
}

func TestTaskIDPrefersWhicheverDispatchPathIsLive(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    string
	}{
		// A cloud print populates task_id and zeroes lan_task_id.
		"cloud": {`{"print":{"task_id":"1180941693","lan_task_id":"0"}}`, "1180941693"},
		// A LAN print does the reverse. Reading only task_id would silently
		// break resume for every LAN print.
		"lan":  {`{"print":{"task_id":"0","lan_task_id":"77"}}`, "77"},
		"none": {`{"print":{"task_id":"0","lan_task_id":"0"}}`, ""},
		"job":  {`{"print":{"job_id":"42"}}`, "42"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewState()
			s.Merge([]byte(tc.payload))
			if got := s.TaskID(); got != tc.want {
				t.Fatalf("TaskID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNumericFieldsTolerateStrings(t *testing.T) {
	s := NewState()
	s.Merge([]byte(`{"print":{"layer_num":"37","nozzle_temper":"219.5"}}`))
	if got := s.Layer(); got != 37 {
		t.Fatalf("layer = %d, want 37", got)
	}
	if got := s.Nozzle(); got != 219.5 {
		t.Fatalf("nozzle = %v, want 219.5", got)
	}
}

func TestFilamentDeduplicates(t *testing.T) {
	s := NewState()
	s.Merge([]byte(`{"print":{"ams":{"ams":[{"tray":[
		{"tray_type":"PLA","tray_color":"0086D6FF"},
		{"tray_type":"PLA","tray_color":"0086D6FF"},
		{"tray_type":"PETG","tray_color":"161616FF"},
		{"tray_type":""}
	]}]}}}`))
	// Four slots of the same PLA should read as one entry, not four.
	if got, want := s.Filament(), "PLA #0086D6, PETG #161616"; got != want {
		t.Fatalf("Filament() = %q, want %q", got, want)
	}
}
