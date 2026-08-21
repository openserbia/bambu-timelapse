package probe

import (
	"context"
	"strings"
	"testing"
)

// cancelled makes every dial fail at once, which is the unreachable printer
// without waiting out a real timeout.
func cancelled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func TestUnreachablePrinterSkipsTheRestOfTheChecks(t *testing.T) {
	// An access code cannot be refused by a printer that never answered, and
	// reporting it as wrong sends the operator after the wrong setting.
	results := Printer(cancelled(t), "printer.invalid", "code", nil, "")

	if len(results) != 4 {
		t.Fatalf("expected all four checks reported, got %+v", results)
	}
	for _, r := range results {
		if r.OK {
			t.Fatalf("check %q passed against an unreachable printer: %s", r.Name, r.Detail)
		}
	}
	if !strings.Contains(results[0].Detail, "PRINTER_HOST") {
		t.Fatalf("the unreachable check does not name the setting to fix: %s", results[0].Detail)
	}
}

func TestSkippedChecksSayWhyRatherThanFailing(t *testing.T) {
	results := Printer(cancelled(t), "printer.invalid", "code", nil, "")

	for _, r := range results[1:] {
		if !strings.Contains(r.Detail, "printer.invalid") {
			t.Fatalf("check %q does not say which host went unanswered: %s", r.Name, r.Detail)
		}
	}
}
