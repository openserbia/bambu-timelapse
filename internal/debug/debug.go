// Package debug implements the one-shot diagnostic dump.
//
// It exists because every question this service raises — is the camera on, is
// LAN liveview enabled, which task id is live, does the printer even have
// storage — is answered by data the printer already publishes. Reading it
// should not require writing a throwaway MQTT client each time.
package debug

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/openserbia/bambu-timelapse/internal/camera"
	"github.com/openserbia/bambu-timelapse/internal/config"
	"github.com/openserbia/bambu-timelapse/internal/ftps"
	"github.com/openserbia/bambu-timelapse/internal/telemetry"
)

const (
	mqttPort      = "8883"
	dialTimeout   = 3 * time.Second
	connectWait   = 15 * time.Second
	defaultWait   = 20 * time.Second
	grabTimeout   = 25 * time.Second
	messagesToSee = 2
	// ftpsTimeout bounds the file-store probe; it is a diagnostic, not a
	// transfer, and a printer that is slow to answer has answered.
	ftpsTimeout = 10 * time.Second
	// disconnectGraceMS lets the broker see a clean DISCONNECT rather than a
	// dropped socket.
	disconnectGraceMS = 500
)

// report accumulates writes and keeps the first error, so a long run of
// output checks its destination once rather than at every line.
type report struct {
	w   io.Writer
	err error
}

func (r *report) printf(format string, args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintf(r.w, format, args...)
}

func (r *report) println(args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintln(r.w, args...)
}

// Options tunes one debug run.
type Options struct {
	// Raw dumps the entire merged report as JSON instead of a summary.
	Raw bool
	// Wait is how long to listen before giving up on a snapshot.
	Wait time.Duration
	// Frame, when set, grabs one camera still to this path.
	Frame string
}

type probe struct {
	port string
	name string
	note string
}

var probes = []probe{
	{"8883", "MQTT/TLS", "telemetry"},
	{"322", "RTSPS", "camera — closed until LAN Only Liveview is enabled"},
	{"990", "FTPS", "file store"},
	{"6000", "chamber-image", "P1/A1 protocol; vestigial on P2/H2 series"},
}

// Run performs the dump against the configured printer.
func Run(ctx context.Context, cfg *config.Config, opts Options, out io.Writer) error {
	rw := &report{w: out}
	if opts.Wait <= 0 {
		opts.Wait = defaultWait
	}
	rw.printf("printer   %s\nserial    %s\n\n", cfg.Host, cfg.Serial)

	tools := camera.NewTools(cfg.FFmpegBin, cfg.FFprobeBin)

	rw.println("== toolchain ==")
	toolchain(ctx, tools, rw)

	rw.println("\n== ports ==")
	for _, p := range probes {
		status := "open"
		dialer := &net.Dialer{Timeout: dialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, p.port))
		if err != nil {
			status = "CLOSED"
		} else {
			_ = conn.Close()
		}
		rw.printf("  %-5s %-14s %-7s %s\n", p.port, p.name, status, p.note)
	}

	rw.println("\n== file store ==")
	files(ctx, cfg, rw)

	rw.printf("\n== telemetry (waiting up to %s) ==\n", opts.Wait)
	state, err := snapshot(ctx, cfg, opts.Wait)
	if err != nil {
		return err
	}
	raw := state.Fields()

	if opts.Raw {
		return dumpRaw(rw, raw)
	}
	summarise(rw, state, raw)

	if opts.Frame != "" {
		rw.println("\n== camera ==")
		grabFrame(ctx, cfg, tools, opts.Frame, rw)
	}
	return rw.err
}

// toolchain reports what the host gives ffmpeg-dependent work to run on. It
// is the same probe the service makes at startup, printed rather than logged:
// "why is there no caption on my timelapse" is answered here or by reading
// hours of logs.
func toolchain(ctx context.Context, tools camera.Tools, rw *report) {
	sup := tools.Detect(ctx)
	for _, tool := range []struct{ name, path string }{
		{"ffmpeg", sup.FFmpeg},
		{"ffprobe", sup.FFprobe},
	} {
		if tool.path == "" {
			rw.printf("  %-9s MISSING\n", tool.name)
			continue
		}
		rw.printf("  %-9s %s\n", tool.name, tool.path)
	}
	for _, filter := range []string{
		camera.FilterDrawtext, camera.FilterSendcmd,
		camera.FilterTpad, camera.FilterConcat,
	} {
		status := "MISSING"
		if sup.Has(filter) {
			status = "ok"
		}
		rw.printf("  %-9s %s\n", filter, status)
	}
}

// files reports what the printer is holding, which is what decides whether a
// plate preview can be fetched. A cloud print keeps its 3mf only while it is
// printing, so an empty store on an idle printer is the normal answer and a
// print in progress is when this is worth asking.
func files(ctx context.Context, cfg *config.Config, rw *report) {
	client, err := ftps.Dial(ctx, cfg.Host, cfg.AccessCode, ftpsTimeout)
	if err != nil {
		rw.printf("  unreachable: %v\n", err)
		return
	}
	defer func() { _ = client.Close() }()

	found := 0
	for _, dir := range []string{"/", "/cache/"} {
		lines, err := client.List(ctx, dir, ftpsTimeout)
		if err != nil {
			rw.printf("  %-9s %v\n", dir, err)
			continue
		}
		for _, line := range lines {
			rw.printf("  %-9s %s\n", dir, line)
			found++
		}
	}
	if found == 0 {
		rw.println("  empty — no 3mf to take a plate preview from")
	}
}

func snapshot(ctx context.Context, cfg *config.Config, wait time.Duration) (*telemetry.State, error) {
	state := telemetry.NewState()
	got := make(chan struct{}, 1)

	opts := mqtt.NewClientOptions().
		AddBroker("ssl://" + net.JoinHostPort(cfg.Host, mqttPort)).
		SetClientID(fmt.Sprintf("bambu-debug-%d", time.Now().UnixNano())).
		SetUsername("bblp").
		SetPassword(cfg.AccessCode).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}). //nolint:gosec // self-signed cert for the printer's own LAN address
		SetConnectTimeout(connectWait).
		SetOrderMatters(true)

	opts.OnConnect = func(c mqtt.Client) {
		c.Subscribe(fmt.Sprintf("device/%s/report", cfg.Serial), 0,
			func(_ mqtt.Client, m mqtt.Message) {
				if state.Merge(m.Payload()) {
					select {
					case got <- struct{}{}:
					default:
					}
				}
			})
		// Without pushall the first message is often a tiny delta and the
		// summary would read as mostly empty for up to a minute.
		c.Publish(fmt.Sprintf("device/%s/request", cfg.Serial), 0, false,
			`{"pushing":{"sequence_id":"0","command":"pushall"}}`)
	}

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(connectWait) {
		// A timeout leaves the token with no error to wrap, which printed as
		// "mqtt connect: %!w(<nil>)" — the least useful line in a tool whose
		// whole job is telling you why the printer is unreachable.
		return nil, fmt.Errorf("mqtt connect: no answer from %s within %s",
			cfg.Host, connectWait)
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("mqtt connect: %w", err)
	}
	defer client.Disconnect(disconnectGraceMS)

	deadline := time.After(wait)
	seen := 0
	for {
		select {
		case <-got:
			seen++
			if seen >= messagesToSee {
				return state, nil
			}
		case <-deadline:
			if seen == 0 {
				return nil, fmt.Errorf("no report within %s", wait)
			}
			return state, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func dumpRaw(rw *report, raw map[string]any) error {
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	rw.println(string(body))
	return rw.err
}

func summarise(rw *report, state *telemetry.State, raw map[string]any) {
	rw.printf("  state          %s\n", state.GcodeState())
	rw.printf("  job            %s\n", state.JobName())
	rw.printf("  task id        %s   (resume key)\n", state.TaskID())
	rw.printf("  layer          %d / %d  (%d%%)\n",
		state.Layer(), state.TotalLayers(), state.Progress())
	rw.printf("  nozzle / bed   %.0f°C / %.0f°C\n", state.Nozzle(), state.Bed())
	rw.printf("  filament       %s\n", state.Filament())

	rw.println("\n== camera & storage ==")
	if cam, ok := raw["ipcam"].(map[string]any); ok {
		// rtsp_url is the oracle for LAN liveview: the literal string
		// "disable" until the printer setting is on, then a URL.
		rw.printf("  rtsp_url       %v\n", cam["rtsp_url"])
		rw.printf("  resolution     %v\n", cam["resolution"])
		rw.printf("  timelapse      %v  (printer's own recorder)\n", cam["timelapse"])
		rw.printf("  internal store %v KB free of %v\n",
			cam["tl_internal_free_kb"], cam["tl_internal_total_kb"])
		rw.printf("  external store %v KB total  (0 = no SD card)\n",
			cam["tl_external_total_kb"])
	}
	rw.printf("  sdcard         %v\n", raw["sdcard"])

	var nested, scalar []string
	for k, v := range raw {
		switch v.(type) {
		case map[string]any, []any:
			nested = append(nested, k)
		default:
			scalar = append(scalar, k)
		}
	}
	sort.Strings(nested)
	rw.println("\n== sections present ==")
	rw.printf("  nested (%d): %v\n", len(nested), nested)
	rw.printf("  scalar (%d keys) — use -raw for the full dump\n", len(scalar))

	// The most-asked question, answered up front: nothing in the report
	// carries toolhead X/Y, so capture cannot be gated on head position.
	rw.println("\n  note: no toolhead X/Y is reported — capture cannot be gated on head position")
}

func grabFrame(ctx context.Context, cfg *config.Config, tools camera.Tools, path string, rw *report) {
	cam := camera.New(cfg.Host, cfg.AccessCode, grabTimeout, tools)
	if err := cam.Grab(ctx, path); err != nil {
		rw.printf("  grab FAILED: %v\n", err)
		rw.println("  (if 322 is closed, enable LAN Only Liveview on the printer)")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w, h := tools.Dimensions(ctx, path)
	rw.printf("  wrote %s (%d bytes, %dx%d)\n", filepath.Clean(path), info.Size(), w, h)
}
