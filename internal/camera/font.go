package camera

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/image/font/gofont/goregular"
)

const (
	// bundledFont is the name the font is materialised under. drawtext takes
	// a path rather than bytes, so the binary's own copy has to reach disk
	// before it can be drawn with.
	bundledFont = "overlay-font.ttf"
	fontPerm    = 0o600
)

// BundledFont writes the font compiled into this binary into dir and returns
// its path, reusing the file if it is already there.
//
// The font travels inside the binary because the alternative — a system
// package the runtime image happens to carry — made a caption into a boot
// dependency: a host without it could not start the service at all, and what
// it was blocking is decoration on a video that would otherwise be fine.
func BundledFont(dir string) (string, error) {
	path := filepath.Join(dir, bundledFont)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(goregular.TTF)) {
		return path, nil
	}
	if err := os.WriteFile(path, goregular.TTF, fontPerm); err != nil {
		return "", fmt.Errorf("write bundled font: %w", err)
	}
	return path, nil
}
