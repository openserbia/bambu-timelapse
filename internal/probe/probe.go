// Package probe answers the four questions every failure of this service
// starts with: is the printer there, does the access code work, is LAN Only
// Liveview on, and does a frame actually come back.
//
// It lives on its own because both the daemon's preflight and the debug dump
// ask them, and the answers are worth nothing if the two disagree.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/openserbia/bambu-timelapse/internal/camera"
)

const (
	// MQTTPort carries telemetry; RTSPSPort carries the chamber camera and
	// stays closed until LAN Only Liveview is enabled on the printer.
	MQTTPort    = "8883"
	RTSPSPort   = "322"
	dialTimeout = 3 * time.Second
	// connectWait bounds the auth handshake. This is a yes/no question, not a
	// session worth waiting out.
	connectWait = 10 * time.Second
	// disconnectGraceMS lets the broker see a clean DISCONNECT rather than a
	// dropped socket.
	disconnectGraceMS = 250
	// framePerm keeps the probe still owner-only, like everything else the
	// service writes.
	framePerm = 0o700
)

// Names are stable so a log line or a grep finds the same check in both
// callers.
const (
	CheckPrinter  = "printer"
	CheckAccess   = "access"
	CheckLiveview = "liveview"
	CheckCamera   = "camera"
)

// Result is one answer, phrased so it reads the same logged or printed.
type Result struct {
	Name   string
	Detail string
	OK     bool
}

// Printer asks all four, in the order in which one answer makes the next
// worth asking: an unreachable printer cannot refuse an access code, and a
// refused access code says nothing about the camera.
//
// frame, when set, is where the test still is kept; otherwise it is taken to
// a temporary file and discarded. A nil cam skips the grab, since the caller
// has already reported the missing ffmpeg and does not need it twice.
func Printer(ctx context.Context, host, accessCode string, cam *camera.Camera, frame string) []Result {
	results := []Result{reachable(ctx, host)}
	if !results[0].OK {
		return append(results,
			skipped(CheckAccess, host),
			skipped(CheckLiveview, host),
			skipped(CheckCamera, host))
	}

	results = append(results, access(host, accessCode), liveview(ctx, host))
	if cam == nil {
		return results
	}
	return append(results, grab(ctx, cam, frame))
}

// reachable is the plainest question there is: does something answer on the
// telemetry port at this address. A typo'd PRINTER_HOST and a printer that is
// off look identical from here, and both are worth saying before anything
// else is attempted.
func reachable(ctx context.Context, host string) Result {
	if err := dial(ctx, host, MQTTPort); err != nil {
		return Result{Name: CheckPrinter, Detail: fmt.Sprintf(
			"no answer from %s on %s: %v (wrong PRINTER_HOST, or the printer is off)",
			host, MQTTPort, err)}
	}
	return Result{Name: CheckPrinter, Detail: net.JoinHostPort(host, MQTTPort), OK: true}
}

// access proves the LAN access code, which is the one credential this service
// has and the one thing a printer will not tell you is wrong until you use
// it. The broker refuses the CONNECT outright, so no subscription is needed.
func access(host, accessCode string) Result {
	opts := mqtt.NewClientOptions().
		AddBroker("ssl://" + net.JoinHostPort(host, MQTTPort)).
		SetClientID(fmt.Sprintf("bambu-probe-%d", time.Now().UnixNano())).
		SetUsername("bblp").
		SetPassword(accessCode).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}). //nolint:gosec // self-signed cert on the printer's own LAN address
		SetConnectTimeout(connectWait)

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(connectWait) {
		return Result{Name: CheckAccess, Detail: fmt.Sprintf(
			"no answer to the MQTT handshake within %s", connectWait)}
	}
	if err := token.Error(); err != nil {
		return Result{Name: CheckAccess, Detail: fmt.Sprintf(
			"refused: %v (check PRINTER_ACCESS_CODE on the printer's network screen)", err)}
	}
	client.Disconnect(disconnectGraceMS)
	return Result{Name: CheckAccess, Detail: "access code accepted", OK: true}
}

// liveview reads the printer setting off the port itself. 322 is closed
// entirely until LAN Only Liveview is enabled, which makes an open socket the
// answer rather than evidence for it.
func liveview(ctx context.Context, host string) Result {
	if err := dial(ctx, host, RTSPSPort); err != nil {
		return Result{Name: CheckLiveview, Detail: fmt.Sprintf(
			"%s closed: %v; enable LAN Only Liveview on the printer",
			RTSPSPort, err)}
	}
	return Result{Name: CheckLiveview, Detail: "LAN Only Liveview is enabled", OK: true}
}

// grab is the only check that proves the thing the service actually does. An
// open 322 that hands back no frame is a real state: another viewer holds the
// stream, or the access code is stale on the camera side alone.
func grab(ctx context.Context, cam *camera.Camera, frame string) Result {
	dest := frame
	if dest == "" {
		dir, err := os.MkdirTemp("", "bambu-probe")
		if err != nil {
			return Result{Name: CheckCamera, Detail: fmt.Sprintf("no temp dir to grab into: %v", err)}
		}
		defer func() { _ = os.RemoveAll(dir) }()
		dest = filepath.Join(dir, "probe.jpg")
	} else if err := os.MkdirAll(filepath.Dir(dest), framePerm); err != nil {
		return Result{Name: CheckCamera, Detail: fmt.Sprintf("%s: %v", dest, err)}
	}

	if err := cam.Grab(ctx, dest); err != nil {
		return Result{Name: CheckCamera, Detail: fmt.Sprintf("no frame: %v", err)}
	}
	info, err := os.Stat(dest)
	if err != nil {
		return Result{Name: CheckCamera, Detail: fmt.Sprintf("grabbed but unreadable: %v", err)}
	}
	if frame == "" {
		return Result{Name: CheckCamera, Detail: fmt.Sprintf("live view returns frames (%d bytes)", info.Size()), OK: true}
	}
	return Result{Name: CheckCamera, Detail: fmt.Sprintf("%s (%d bytes)", filepath.Clean(dest), info.Size()), OK: true}
}

// skipped says why a question was not asked, which is more use than an
// invented failure for it.
func skipped(name, host string) Result {
	return Result{Name: name, Detail: "not checked; " + host + " is unreachable"}
}

func dial(ctx context.Context, host, port string) error {
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	return conn.Close()
}
