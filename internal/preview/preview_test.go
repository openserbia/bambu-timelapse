package preview

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestPlateNumberComesFromTheRunningFile(t *testing.T) {
	cases := map[string]int{
		"/data/Metadata/plate_1.gcode": 1,
		"/data/Metadata/plate_3.gcode": 3,
		// A multi-plate 3mf carries a render per plate, and only one of them
		// is being printed.
		"Metadata/plate_12.gcode": 12,
		// Nothing to read: plate 1 is the only sensible guess.
		"":              1,
		"whatever.bgc":  1,
		"plate_0.gcode": 1,
	}
	for file, want := range cases {
		if got := plateNumber(file); got != want {
			t.Errorf("plateNumber(%q) = %d, want %d", file, got, want)
		}
	}
}

func TestPickPlatePrefersTheFullRenderOfThePrintedPlate(t *testing.T) {
	names := []string{
		"3D/3dmodel.model",
		"Metadata/plate_1.png",
		"Metadata/plate_1_small.png",
		"Metadata/plate_2.png",
		"Metadata/top_1.png",
	}
	if got := pickPlate(names, 2); got != "Metadata/plate_2.png" {
		t.Errorf("pickPlate(plate 2) = %q", got)
	}
	// The _small variant is 128px and would be scaled into mush beside 1080p
	// footage, so it is never the answer.
	if got := pickPlate([]string{"Metadata/plate_1_small.png"}, 1); got != "" {
		t.Errorf("pickPlate chose a thumbnail: %q", got)
	}
	// A plate that is not in the archive falls back to one that is, rather
	// than to nothing.
	if got := pickPlate(names, 9); got != "Metadata/plate_1.png" {
		t.Errorf("pickPlate(missing plate) = %q", got)
	}
}

func TestListedFileReadsTheNameOffAnLsLine(t *testing.T) {
	cases := map[string]string{
		"-rw-r--r--    1 root  root  1048576 Aug 20 18:22 Clip.3mf": "Clip.3mf",
		"drwxr-xr-x    2 root  root     4096 Aug 20 18:22 cache":    "cache",
		"": "",
	}
	for line, want := range cases {
		if got := listedFile(line); got != want {
			t.Errorf("listedFile(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestPlateImageReadsThePngOutOfThe3mf(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"3D/3dmodel.model":           "<model/>",
		"Metadata/plate_1.png":       "PLATE-ONE",
		"Metadata/plate_2.png":       "PLATE-TWO",
		"Metadata/plate_2_small.png": "thumb",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := plateImage(buf.Bytes(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PLATE-TWO" {
		t.Fatalf("plateImage returned %q", got)
	}
}

func TestPlateImageOnSomethingThatIsNotA3mf(t *testing.T) {
	if _, err := plateImage([]byte("not a zip"), 1); err == nil {
		t.Fatal("a corrupt archive was accepted")
	}
}
