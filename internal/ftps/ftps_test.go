package ftps

import (
	"strings"
	"testing"
)

func TestPasvAddrUsesTheDialledHost(t *testing.T) {
	// The address in the reply is the one the printer believes it has, which
	// is not necessarily the one we reached it on.
	got, err := pasvAddr("192.168.1.110", "227 Entering Passive Mode (10,0,0,5,195,91).\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "192.168.1.110:50011"; got != want {
		t.Fatalf("pasvAddr = %q, want %q", got, want)
	}
}

func TestPasvAddrRejectsNonsense(t *testing.T) {
	for _, reply := range []string{
		"227 Entering Passive Mode\r\n",
		"227 (1,2,3)\r\n",
		"500 Unknown command\r\n",
		"227 (a,b,c,d,e,f)\r\n",
	} {
		if _, err := pasvAddr("host", reply); err == nil {
			t.Errorf("accepted %q", strings.TrimSpace(reply))
		}
	}
}

func TestFinalDistinguishesContinuations(t *testing.T) {
	if final("230-Welcome to the printer\r\n") {
		t.Error("a continuation line was read as the end of the reply")
	}
	if !final("230 Login successful.\r\n") {
		t.Error("the last line was not recognised")
	}
}

func TestRedactKeepsTheAccessCodeOutOfErrors(t *testing.T) {
	// Errors are logged, and the access code is the password to MQTT, the
	// camera and the file store alike.
	if got := redact("PASS 12345678"); strings.Contains(got, "12345678") {
		t.Fatalf("redact leaked the code: %q", got)
	}
	if got := redact("LIST /cache/"); got != "LIST /cache/" {
		t.Fatalf("redact mangled an ordinary command: %q", got)
	}
}
