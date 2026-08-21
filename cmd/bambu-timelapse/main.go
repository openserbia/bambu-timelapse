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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "debug":
			return runDebug(os.Args[2:])
		case "record":
			return runRecord(os.Args[2:])
		}
	}

	log := newLogger()

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

// newLogger honours LOG_LEVEL, falling back to info on anything unreadable.
// A typo in a log level is not a reason to refuse to run.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// runRecord captures a print and keeps it, rather than posting it.
//
// It needs no destination for the same reason debug needs none: checking that
// a crop frames the plate or that a caption reads right should not require a
// chat on the other end, or put a test print in one.
func runRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	out := fs.String("out", ".", "directory to write the finished video to")
	staging := fs.String("staging", "./staging", "where frames are kept while capturing")
	once := fs.Bool("once", false, "stop after the first print is recorded")
	every := fs.Duration("interval", 0,
		"capture on this interval instead of on layer changes; works with an idle printer")
	frames := fs.Int("frames", 0, "with -interval, stop after this many frames")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	log := newLogger()
	cfg, err := config.LoadPrinter()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return 1
	}
	// An empty APIURL is what puts the service in local mode; LoadPrinter has
	// already left it that way.
	cfg.OutputDir = *out
	cfg.StagingDir = *staging
	cfg.Once = *once

	svc, err := service.New(cfg, log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("recording", "host", cfg.Host, "out", *out, "staging", *staging, "once", *once)

	run := svc.Run
	if *every > 0 {
		// Timed capture needs no telemetry and no print: it is how the crop,
		// the caption and the encode are checked without waiting hours for a
		// job to finish.
		run = func(ctx context.Context) error {
			return svc.RunTimed(ctx, *every, *frames)
		}
	}
	if err := run(ctx); err != nil {
		log.Error("recording failed", "err", err)
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
