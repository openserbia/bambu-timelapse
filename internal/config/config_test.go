package config

import (
	"strings"
	"testing"
	"time"
)

// setPrinterEnv sets the minimum a printer-only load needs.
func setPrinterEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"PRINTER_HOST":        "192.168.1.110",
		"PRINTER_SERIAL":      "SERIAL123",
		"PRINTER_ACCESS_CODE": "code",
	} {
		t.Setenv(k, v)
	}
}

func TestCropIsValidatedAtStartup(t *testing.T) {
	// The encode runs once, at the END of a print. A crop typo caught by
	// ffmpeg would surface hours later with every frame already captured and
	// nothing to show for them, so it has to fail at boot instead.
	bad := []string{"1920x820", "1920:820:0", "top-band", "1920:820:0:260:1", "-1:8:0:0"}
	for _, spec := range bad {
		t.Run(spec, func(t *testing.T) {
			setPrinterEnv(t)
			t.Setenv("CROP", spec)
			if _, err := LoadPrinter(); err == nil || !strings.Contains(err.Error(), "CROP") {
				t.Fatalf("CROP=%q accepted; err=%v", spec, err)
			}
		})
	}
}

func TestCropAccepted(t *testing.T) {
	setPrinterEnv(t)
	t.Setenv("CROP", "1920:820:0:260")
	cfg, err := LoadPrinter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Crop != "1920:820:0:260" {
		t.Fatalf("Crop = %q", cfg.Crop)
	}
}

func TestCropEmptyByDefault(t *testing.T) {
	setPrinterEnv(t)
	t.Setenv("CROP", "")
	cfg, err := LoadPrinter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Crop != "" {
		t.Fatalf("Crop defaulted to %q; the frame should be left whole", cfg.Crop)
	}
}

func TestCaptureDelay(t *testing.T) {
	setPrinterEnv(t)
	t.Setenv("CAPTURE_DELAY", "2")
	cfg, err := LoadPrinter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CaptureDelay != 2*time.Second {
		t.Fatalf("CaptureDelay = %v, want 2s", cfg.CaptureDelay)
	}

	t.Setenv("CAPTURE_DELAY", "-1")
	if _, err := LoadPrinter(); err == nil {
		t.Fatal("a negative CAPTURE_DELAY must be rejected")
	}
}

func TestPrinterLoadDoesNotRequireASink(t *testing.T) {
	setPrinterEnv(t)
	for _, k := range []string{"MEDIA_API_URL", "MEDIA_API_TOKEN"} {
		t.Setenv(k, "")
	}
	// Diagnostics must not be gated on the publish destination.
	if _, err := LoadPrinter(); err != nil {
		t.Fatalf("LoadPrinter should not need a sink: %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("the full Load must still require a sink")
	}
}

func TestErrorsAreCollectedNotShortCircuited(t *testing.T) {
	for _, k := range []string{"PRINTER_HOST", "PRINTER_SERIAL", "PRINTER_ACCESS_CODE"} {
		t.Setenv(k, "")
	}
	_, err := LoadPrinter()
	if err == nil {
		t.Fatal("expected errors")
	}
	// One restart should be enough to see everything that is wrong.
	for _, want := range []string{"PRINTER_HOST", "PRINTER_SERIAL", "PRINTER_ACCESS_CODE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in: %v", want, err)
		}
	}
}
