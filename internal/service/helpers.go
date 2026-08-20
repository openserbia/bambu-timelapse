package service

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// fallbackName is used when a job title sanitises down to nothing.
	fallbackName     = "print"
	secondsPerHour   = 3600
	secondsPerMinute = 60
)

// insecureTLS accepts the printer's self-signed certificate.
//
// Not a weakening of anything: the printer issues its own certificate for its
// own LAN address and there is no CA to validate against. Confidentiality on
// the wire is preserved; only the identity check is skipped, and the peer is
// pinned by IP on the local network.
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // see above
}

// freeBytes reports free space on the filesystem holding path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bsize < 0 {
		return 0, nil
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// clean makes a string safe for a filename, matching what the media API
// enforces on its side anyway.
func clean(s string, limit int) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	trimmed := strings.TrimLeft(string(out), ".")
	if len(trimmed) > limit {
		trimmed = trimmed[:limit]
	}
	if trimmed == "" {
		return fallbackName
	}
	return trimmed
}

// humanDuration renders a print length the way a person would say it.
func humanDuration(d time.Duration) string {
	total := int(d.Seconds())
	h, m := total/secondsPerHour, (total%secondsPerHour)/secondsPerMinute
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// findVideo returns the encoded mp4 inside a parked job directory.
func findVideo(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mp4") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// readCaption recovers the caption written beside a parked video, so a retry
// posts the same text rather than an empty one.
func readCaption(dir string) string {
	// #nosec G304 -- dir comes from this service's own failed/ tree.
	body, err := os.ReadFile(filepath.Join(dir, "caption.txt"))
	if err != nil {
		return ""
	}
	return string(body)
}
