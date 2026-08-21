package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateThenReopenIsAResume(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first, resumed, err := store.Create("task-1", "Bracket", 499, 1)
	if err != nil || resumed {
		t.Fatalf("first Create: resumed=%v err=%v", resumed, err)
	}
	first.Frames = 137
	first.LastLayer = 140
	first.Nozzle.Add(220)
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}

	// A restart mid-print: same task id, so the state must come back rather
	// than a second session starting for the same print.
	again, resumed, err := store.Create("task-1", "Bracket", 499, 140)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("second Create did not report a resume")
	}
	if again.Frames != 137 || again.LastLayer != 140 {
		t.Fatalf("state lost across resume: frames=%d last_layer=%d", again.Frames, again.LastLayer)
	}
	if !again.StartedAt.Equal(first.StartedAt) {
		t.Fatal("StartedAt changed on resume; the caption's duration would be wrong")
	}
	if again.Nozzle.Count != 1 || again.Nozzle.Peak != 220 {
		t.Fatalf("temperature stats lost: %+v", again.Nozzle)
	}
}

func TestPartialFlagSetWhenCaptureStartsMidPrint(t *testing.T) {
	store, _ := NewStore(t.TempDir())

	fresh, _, _ := store.Create("a", "job", 100, 1)
	if fresh.Partial {
		t.Fatal("a print seen from layer 1 is not partial")
	}
	mid, _, _ := store.Create("b", "job", 100, 250)
	if !mid.Partial {
		t.Fatal("a print first seen at layer 250 must be marked partial")
	}
}

func TestFramePathIsSequentialNotLayerNumbered(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	s, _, _ := store.Create("t", "job", 10, 1)

	// Numbering must be gap-free regardless of which layers produced frames:
	// ffmpeg's image2 demuxer stops at the first missing index.
	if got := filepath.Base(s.FramePath()); got != "frame-00001.jpg" {
		t.Fatalf("first frame = %q", got)
	}
	s.Frames = 41
	if got := filepath.Base(s.FramePath()); got != "frame-00042.jpg" {
		t.Fatalf("42nd frame = %q", got)
	}
}

func TestListAndDiscard(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	a, _, _ := store.Create("a", "one", 1, 1)
	b, _, _ := store.Create("b", "two", 1, 1)

	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List() returned %d sessions, want 2", len(got))
	}

	store.Discard(a)
	if got, _ = store.List(); len(got) != 1 || got[0].TaskID != b.TaskID {
		t.Fatalf("after Discard: %+v", got)
	}
}

func TestParkAndSweep(t *testing.T) {
	root := t.TempDir()
	store, _ := NewStore(root)
	s, _, _ := store.Create("t", "job", 1, 1)

	if err := store.Park(s, "upload-failed"); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := store.List(); len(sessions) != 0 {
		t.Fatal("a parked session must leave the active staging area")
	}
	parked, _ := os.ReadDir(store.FailedDir())
	if len(parked) != 1 {
		t.Fatalf("failed dir holds %d entries, want 1", len(parked))
	}

	// Fresh: must survive its TTL.
	if n := store.SweepFailed(time.Hour); n != 0 {
		t.Fatalf("swept %d fresh entries", n)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(store.FailedDir(), parked[0].Name()), old, old)
	if n := store.SweepFailed(24 * time.Hour); n != 1 {
		t.Fatalf("swept %d expired entries, want 1", n)
	}
}

func TestTempsIgnoreIdleZeroes(t *testing.T) {
	var temps Temps
	temps.Add(0)
	temps.Add(-5)
	if temps.Count != 0 || temps.Avg() != 0 {
		t.Fatalf("idle zeroes polluted the average: %+v", temps)
	}
	temps.Add(200)
	temps.Add(220)
	if temps.Avg() != 210 || temps.Peak != 220 {
		t.Fatalf("avg=%v peak=%v", temps.Avg(), temps.Peak)
	}
}
