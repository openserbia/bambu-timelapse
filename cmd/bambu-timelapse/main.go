// Command bambu-timelapse records layer-synced timelapses of Bambu Lab prints
// from the printer's own chamber camera and posts them to Telegram.
//
// It uses no cloud service and none of the printer's storage: telemetry comes
// off MQTT on 8883, frames off RTSPS on 322, both authenticated with the LAN
// access code. A frame is taken on every layer change, so the result is layer
// synced rather than time-lapsed on a wall clock.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openserbia/bambu-timelapse/internal/config"
	"github.com/openserbia/bambu-timelapse/internal/debug"
	"github.com/openserbia/bambu-timelapse/internal/service"
)

// defaultDebugWait bounds how long `debug` listens for a snapshot.
const defaultDebugWait = 20 * time.Second

// exitUsage is the conventional shell exit code for a bad invocation, kept
// distinct from exitError so a wrapper can tell the two apart.
const exitUsage = 2

func main() {
	// run() rather than exiting inline: os.Exit skips deferred calls, so the
	// signal handler's stop() would never run.
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 && os.Args[1] == "debug" {
		return runDebug(os.Args[2:])
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return 1
	}

	svc, err := service.New(cfg, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := svc.Serve(ctx); err != nil {
			// A bound-port failure is fatal in substance: without /healthz the
			// container is unmonitorable, so fail loudly rather than run blind.
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	log.Info("watching printer",
		"host", cfg.Host, "serial", cfg.Serial, "name", cfg.PrinterName)
	if err := svc.Run(ctx); err != nil {
		log.Error("service exited with error", "err", err)
		return 1
	}
	return 0
}

// runDebug prints one diagnostic snapshot and exits. It needs only the printer
// half of the configuration: refusing to run without MEDIA_API_URL when the
// question is "why can I not reach the printer" gets the order backwards.
func runDebug(args []string) int {
	fs := flag.NewFlagSet("debug", flag.ContinueOnError)
	raw := fs.Bool("raw", false, "dump the entire merged report as JSON")
	frame := fs.String("frame", "", "also grab one camera still to this path")
	wait := fs.Duration("wait", defaultDebugWait, "how long to wait for a snapshot")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.LoadPrinter()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := debug.Run(ctx, cfg, debug.Options{
		Raw: *raw, Frame: *frame, Wait: *wait,
	}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "debug:", err)
		return 1
	}
	return 0
}
