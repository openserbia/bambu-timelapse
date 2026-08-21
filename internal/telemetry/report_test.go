package telemetry

import "testing"

func TestMergeKeepsFieldsAcrossDeltas(t *testing.T) {
	s := NewState()
	if !s.Merge([]byte(`{"print":{"gcode_state":"RUNNING","layer_num":10,"total_layer_num":499}}`)) {
		t.Fatal("first report rejected")
	}
	// The printer sends deltas. A message carrying only layer_num must not
	// erase gcode_state. Assigning instead of merging would make every such
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

func TestNewJobForgetsThePreviousJobsLayers(t *testing.T) {
	// The failure this prevents: a 60-layer print leaves layer_num 60 in the
	// merged view, the next print has 13 layers, every "has the layer
	// advanced" test fails for its whole duration, and the job is captured
	// once and discarded for having too few frames.
	state := NewState()
	state.Merge([]byte(`{"print":{"task_id":"111","layer_num":60,"total_layer_num":60,"subtask_name":"tall","nozzle_temper":220.0}}`))

	state.Merge([]byte(`{"print":{"task_id":"222","gcode_state":"RUNNING"}}`))

	if got := state.Layer(); got != 0 {
		t.Errorf("layer_num = %d after a new job started; want it forgotten", got)
	}
	if got := state.TotalLayers(); got != 0 {
		t.Errorf("total_layer_num = %d after a new job started", got)
	}
	if got := state.JobName(); got != "" {
		t.Errorf("subtask_name = %q after a new job started", got)
	}
	// Device-scoped fields are not the job's to invalidate: the printer sends
	// them rarely, and dropping them blinds the service until a full snapshot.
	if got := state.Nozzle(); got != 220.0 {
		t.Errorf("nozzle_temper = %v; device state should survive a job change", got)
	}
}

func TestDeltasWithinAJobStillMerge(t *testing.T) {
	state := NewState()
	state.Merge([]byte(`{"print":{"task_id":"111","layer_num":10,"total_layer_num":60}}`))
	state.Merge([]byte(`{"print":{"layer_num":11}}`))

	if got := state.Layer(); got != 11 {
		t.Errorf("layer_num = %d, want the delta applied", got)
	}
	if got := state.TotalLayers(); got != 60 {
		t.Errorf("total_layer_num = %d; a delta must not wipe it", got)
	}
}

func TestLanJobFollowingACloudJobIsANewJob(t *testing.T) {
	// The ids swap places between dispatch methods, which must still read as
	// "a different print", not as one job losing its identity.
	state := NewState()
	state.Merge([]byte(`{"print":{"task_id":"111","lan_task_id":"0","layer_num":60}}`))
	state.Merge([]byte(`{"print":{"task_id":"0","lan_task_id":"777"}}`))

	if got := state.Layer(); got != 0 {
		t.Errorf("layer_num = %d; a LAN print after a cloud print is a new job", got)
	}
	if got := state.TaskID(); got != "777" {
		t.Errorf("TaskID() = %q, want the LAN id", got)
	}
}
