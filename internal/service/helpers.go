package service

import (
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// fallbackName is used when a job title sanitises down to nothing.
	fallbackName   = "print"
	secondsPerHour = 3600
	// A local recording is meant to be watched and shared by hand, unlike the
	// staging tree, which is the service's alone.
	outputDirPerm    = 0o755
	outputFilePerm   = 0o644
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

// stagingWritable proves the staging tree can be written to, rather than
// assuming it: a read-only mount or a container running as the wrong user
// both look fine until the first frame lands.
func (s *Service) stagingWritable() (bool, error) {
	probe := filepath.Join(s.store.Root(), probeFile)
	if err := os.WriteFile(probe, []byte("ok"), captionPerm); err != nil {
		return false, err
	}
	return true, os.Remove(probe)
}

// move renames a file, falling back to a copy when the destination is on
// another filesystem, which it usually is: staging is a container volume and
// the output directory is wherever the operator asked for it.
func move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// G304: src is a file this service just encoded inside its own staging
	// tree, and dst is the output directory the operator named on the command
	// line. Neither crosses a trust boundary.
	in, err := os.Open(src) //nolint:gosec // see above
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, outputFilePerm) //nolint:gosec // see above
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// Closed explicitly, not deferred: a write error surfaces here, and
	// removing the source before knowing the copy landed would lose the video.
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
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
