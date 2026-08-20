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
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openserbia/bambu-timelapse/internal/config"
	"github.com/openserbia/bambu-timelapse/internal/service"
)

func main() {
	// run() rather than exiting inline: os.Exit skips deferred calls, so the
	// signal handler's stop() would never run.
	os.Exit(run())
}

func run() int {
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
