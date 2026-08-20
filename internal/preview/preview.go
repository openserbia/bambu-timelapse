// Package preview fetches the slicer's render of the job being printed.
//
// The render is what the printer shows on its own screen: the plate as it was
// sliced, before any of it existed. It lives inside the job's 3mf, which for
// a cloud print the printer keeps only while it is printing it — so this is
// worth asking for at the start of a job and pointless afterwards.
package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openserbia/bambu-timelapse/internal/ftps"
)

// previewLimit bounds what is read out of the archive. A plate render is tens
// of kilobytes; this is slack, not a target.
const previewLimit = 8 << 20

// searchDirs are where a 3mf turns up: cloud prints in the root of the
// internal store, LAN uploads under /cache, and the SD card on printers that
// have one. Listing a directory that does not exist is not an error on this
// server, so asking for all of them costs one round trip each.
var searchDirs = []string{"/", "/cache/", "/sdcard/", "/model/"}

// plateFromGcode reads the plate number out of the path the printer reports
// as the running gcode, e.g. /data/Metadata/plate_3.gcode. A multi-plate 3mf
// carries a render per plate and only one of them is being printed.
var plateFromGcode = regexp.MustCompile(`plate_(\d+)\.gcode`)

// listedName is the filename at the end of an LIST line, which is otherwise
// ls -l output nobody needs.
var listedName = regexp.MustCompile(`\s(\S+)$`)

// Fetch returns the PNG the slicer rendered for the plate now printing.
//
// jobName is the printer's own title for the job and is used only to choose
// between several stored 3mfs; gcodeFile is what it reports as the running
// file, and names the plate inside.
func Fetch(ctx context.Context, host, accessCode, jobName, gcodeFile string, timeout time.Duration) ([]byte, error) {
	client, err := ftps.Dial(ctx, host, accessCode, timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	archive, err := findArchive(ctx, client, jobName, timeout)
	if err != nil {
		return nil, err
	}
	body, err := client.Retrieve(ctx, archive, timeout)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", archive, err)
	}
	return plateImage(body, plateNumber(gcodeFile))
}

// findArchive locates the job's 3mf on the printer.
func findArchive(ctx context.Context, client *ftps.Client, jobName string, timeout time.Duration) (string, error) {
	var found []string
	for _, dir := range searchDirs {
		lines, err := client.List(ctx, dir, timeout)
		if err != nil {
			return "", err
		}
		for _, line := range lines {
			name := listedFile(line)
			if strings.EqualFold(path.Ext(name), ".3mf") {
				found = append(found, path.Join(dir, name))
			}
		}
	}
	switch len(found) {
	case 0:
		// Expected, not exceptional: an idle printer that streamed its last
		// job from the cloud keeps nothing at all.
		return "", errors.New("no 3mf on the printer")
	case 1:
		return found[0], nil
	}
	// Several: prefer the one named after the job. The printer's title and
	// the filename are the same string often enough to be worth trying, and
	// picking wrongly shows the wrong render rather than failing.
	for _, candidate := range found {
		if jobName != "" && strings.Contains(
			strings.ToLower(candidate), strings.ToLower(jobName)) {
			return candidate, nil
		}
	}
	return found[0], nil
}

// listedFile pulls the filename off a LIST line.
func listedFile(line string) string {
	match := listedName.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	return match[1]
}

// plateNumber is the plate being printed, or 1 when the printer does not say.
func plateNumber(gcodeFile string) int {
	match := plateFromGcode.FindStringSubmatch(gcodeFile)
	if match == nil {
		return 1
	}
	n, err := strconv.Atoi(match[1])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// plateImage pulls one plate's render out of a 3mf, which is a zip.
func plateImage(archive []byte, plate int) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("3mf: %w", err)
	}
	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	wanted := pickPlate(names, plate)
	if wanted == "" {
		return nil, errors.New("3mf carries no plate render")
	}
	for _, f := range r.File {
		if f.Name != wanted {
			continue
		}
		return readEntry(f)
	}
	return nil, errors.New("3mf carries no plate render")
}

// readEntry reads one zip entry whole, closing it before returning: a defer
// inside the search loop would hold every entry it walked past.
func readEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("3mf %s: %w", f.Name, err)
	}
	body, err := io.ReadAll(io.LimitReader(rc, previewLimit))
	if closeErr := rc.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("3mf %s: %w", f.Name, err)
	}
	return body, nil
}

// pickPlate chooses the render for a plate.
//
// A Bambu 3mf holds several images per plate: the full render, a "_small"
// thumbnail and a top-down view. The full render of the plate being printed
// is the one worth showing; the thumbnail is 128px and would be scaled up
// into mush next to 1080p footage.
func pickPlate(names []string, plate int) string {
	exact := fmt.Sprintf("Metadata/plate_%d.png", plate)
	var fallback string
	for _, name := range names {
		if name == exact {
			return name
		}
		if fallback == "" &&
			strings.HasPrefix(name, "Metadata/plate_") &&
			strings.HasSuffix(name, ".png") &&
			!strings.Contains(name, "_small") {
			fallback = name
		}
	}
	return fallback
}
