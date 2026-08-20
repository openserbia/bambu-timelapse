// Package session owns the on-disk record of a print being captured.
//
// The state file is what makes the service survive a restart. Everything that
// would otherwise live only in memory — the frame counter, the true start
// time, the running temperature statistics — is rewritten after every frame,
// so a Pi that reboots mid-print resumes the same session rather than
// starting a second one and posting two half timelapses.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	stateFile = "state.json"

	// Owner-only: the staging tree is this service's alone, and the state file
	// records what is being printed.
	dirPerm  = 0o750
	filePerm = 0o600
)

// Temps accumulates a running mean and peak.
type Temps struct {
	Sum   float64 `json:"sum"`
	Count int     `json:"count"`
	Peak  float64 `json:"peak"`
}

// Add folds in one sample, ignoring the zeroes the printer reports while idle.
func (t *Temps) Add(v float64) {
	if v <= 0 {
		return
	}
	t.Sum += v
	t.Count++
	if v > t.Peak {
		t.Peak = v
	}
}

// Avg is the mean sample, or zero when nothing was recorded.
func (t *Temps) Avg() float64 {
	if t.Count == 0 {
		return 0
	}
	return t.Sum / float64(t.Count)
}

// Session is one print job's capture state.
type Session struct {
	TaskID      string    `json:"task_id"`
	JobName     string    `json:"job_name"`
	StartedAt   time.Time `json:"started_at"`
	Frames      int       `json:"frames"`
	LastLayer   int       `json:"last_layer"`
	TotalLayers int       `json:"total_layers"`
	// Layers is the layer each captured frame belongs to, in frame order.
	// Frame numbering is sequential and a skipped grab leaves no frame, so
	// this is the only honest way to caption frame N with a layer number —
	// anything derived from N alone drifts by every layer the camera missed.
	Layers   []int  `json:"layers,omitempty"`
	Nozzle   Temps  `json:"nozzle"`
	Bed      Temps  `json:"bed"`
	Filament string `json:"filament"`
	Pauses   int    `json:"pauses"`
	// Partial records that capture began after layer 1 — the service booted
	// mid-print. The caption must say so: reporting elapsed-since-we-started
	// as the print duration is a lie the video itself contradicts.
	Partial bool `json:"partial"`

	dir string
}

// Dir is the session's staging directory.
func (s *Session) Dir() string { return s.dir }

// FramePath is where the next frame belongs.
//
// Numbering is sequential and independent of layer number on purpose: naming
// frames after layers turns every dropped frame into a gap, and ffmpeg's
// image2 demuxer stops dead at the first missing index. A sequential counter
// degrades to "slightly jumpy", which is the failure mode worth having.
func (s *Session) FramePath() string {
	return filepath.Join(s.dir, fmt.Sprintf("frame-%05d.jpg", s.Frames+1))
}

// Save rewrites the state file atomically, so a crash mid-write cannot leave
// a truncated file that fails to parse on the next boot.
func (s *Session) Save() error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, body, filePerm); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, stateFile))
}

// Store manages the staging area.
type Store struct {
	root   string
	failed string
}

// NewStore prepares the staging and failed directories.
func NewStore(root string) (*Store, error) {
	failed := filepath.Join(root, "failed")
	if err := os.MkdirAll(failed, dirPerm); err != nil {
		return nil, err
	}
	return &Store{root: root, failed: failed}, nil
}

// Root is the staging directory.
func (st *Store) Root() string { return st.root }

// FailedDir holds jobs kept for inspection after a failure.
func (st *Store) FailedDir() string { return st.failed }

// Create starts a session, or reopens an existing one for the same task.
// Reopening is what a resume after a restart actually is.
func (st *Store) Create(taskID, jobName string, totalLayers, currentLayer int) (*Session, bool, error) {
	dir := filepath.Join(st.root, "job-"+sanitize(taskID))
	if existing, err := load(dir); err == nil {
		return existing, true, nil
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, false, err
	}
	s := &Session{
		TaskID:      taskID,
		JobName:     jobName,
		StartedAt:   time.Now(),
		LastLayer:   -1,
		TotalLayers: totalLayers,
		// >1 means the print was already under way when we first saw it.
		Partial: currentLayer > 1,
		dir:     dir,
	}
	if err := s.Save(); err != nil {
		return nil, false, err
	}
	return s, false, nil
}

// List returns every session on disk, newest first.
func (st *Store) List() ([]*Session, error) {
	entries, err := os.ReadDir(st.root)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		s, err := load(filepath.Join(st.root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Discard removes a session's directory.
func (st *Store) Discard(s *Session) { _ = os.RemoveAll(s.dir) }

// Park moves a session aside for inspection, bounded by SweepFailed's TTL.
func (st *Store) Park(s *Session, reason string) error {
	dest := filepath.Join(st.failed, reason+"-"+filepath.Base(s.dir))
	if err := os.Rename(s.dir, dest); err != nil {
		_ = os.RemoveAll(s.dir)
		return err
	}
	s.dir = dest
	return nil
}

// SweepFailed deletes parked jobs past their TTL and returns how many went.
func (st *Store) SweepFailed(ttl time.Duration) int {
	entries, err := os.ReadDir(st.failed)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-ttl)
	n := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(filepath.Join(st.failed, e.Name())) == nil {
			n++
		}
	}
	return n
}

func load(dir string) (*Session, error) {
	// #nosec G304 -- dir comes from this service's own staging tree.
	body, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	s.dir = dir
	return &s, nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
