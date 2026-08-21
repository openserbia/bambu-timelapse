# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

A single Go daemon (`cmd/bambu-timelapse`) that watches one Bambu Lab printer
over local MQTT, grabs a still off the RTSPS chamber camera on every layer
change, and at end of print encodes the frames with ffmpeg and POSTs the video
to a generic media API. README.md covers the domain reasoning (why camera
capture instead of the printer's own timelapse, printer setup, every env var);
read it before changing capture, resume, or upload behaviour.

## Commands

Everything runs through devbox + Taskfile. The toolchain (Go, golangci-lint,
gofumpt, govulncheck, gitleaks) is pinned in `devbox.json`, so bare
`go`/`golangci-lint` may be a different version than CI's.

```sh
devbox run -- task ci      # secrets + lint + test + build; what CI runs
devbox run -- task test    # go test ./...
devbox run -- task lint    # deps, gci+gofumpt write, golangci-lint run
devbox run -- task build   # static binary into build/
devbox run -- task secrets # gitleaks over the working tree and history
devbox run -- task hooks   # core.hooksPath -> .githooks; once per clone
devbox run -- task dist VERSION=x.y.z
```

`task lint` reformats in place (gci import grouping + gofumpt), so run it
before proposing a diff rather than hand-formatting imports.

`.githooks/pre-commit` runs `task secrets` on every commit, `task lint` on any
commit touching Go files, and refuses the commit if the formatter rewrote
something: the rewrite lands in the working tree, not the index.

## Layout

| Package | Owns |
| --- | --- |
| `internal/config` | Env parsing and validation; every default lives here as a named constant |
| `internal/telemetry` | Decoding the MQTT report topic into a merged `State` |
| `internal/camera` | ffmpeg/ffprobe: `Grab`, `Encode`, `Crop`, `Dimensions` |
| `internal/session` | The on-disk job (`staging/job-<task_id>/state.json`) and its lifecycle |
| `internal/probe` | The four printer checks (reachable, access, liveview, a real grab) shared by preflight and `debug` |
| `internal/uploader` | The multipart POST and its retryable/terminal error |
| `internal/service` | Wiring: MQTT lifecycle, capture, finalise, `/metrics`, `/healthz` |
| `internal/debug` | The one-shot `debug` subcommand |

Three entry points share the loop: the daemon (posts), `record` (keeps the
file locally; no `MEDIA_API_*` needed), and `debug` (one diagnostic dump).
An empty `APIURL` is what selects local delivery.

`service.go` is the only place the pieces meet; the other packages do not
import each other.

## Invariants worth not breaking

- **Reports are deltas, not snapshots.** `telemetry.State` merges into a map.
  Assigning a decoded struct would wipe `gcode_state` every time the printer
  sends `{"layer_num": N}` alone. The flip side: job-scoped fields must be
  dropped when `task_id` changes, or the previous print's layer count silences
  the next one (`telemetry.jobScoped`).
- **Job identity is `task_id`, falling back to `lan_task_id`.** Cloud prints
  populate one and zero the other; LAN prints do the reverse. Reading only one
  breaks resume for half of all prints.
- **Frames are numbered sequentially, never by layer.** ffmpeg's `image2`
  demuxer stops at the first gap, so a dropped frame must not leave one. The
  layer each frame belongs to is recorded in `session.Layers` instead. That
  list, not the frame index, is what the burned-in counter reads.
- **State is saved after every frame.** Anything that must survive a restart
  (frame counter, true start time, temperature stats) belongs in
  `session.Session`, not in a service field.
- **The uploader knows nothing about its consumer.** Extra form fields come
  from `MEDIA_API_FIELDS` verbatim. Do not add Telegram-specific (or any
  destination-specific) logic here. That was deliberately removed.
- **External dependencies are probed at startup, not at use.** `preflight`
  runs before the first capture; only a missing ffmpeg or an unwritable
  staging tree is fatal, everything else degrades loudly. Adding a new
  dependency on the host means adding a check there. The printer half of it
  lives in `internal/probe` so `debug` reports the same answers; `debug` runs
  those checks and not the ffmpeg ones, because an unreachable printer is not
  a question about filters.
- **The plate preview is fetched during the print, not after.** A cloud job's
  3mf exists on the printer only while it prints; `internal/ftps` exists
  because its vsFTPd needs implicit TLS, a resumed session on the data
  channel, and the transfer command before the handshake.
- **The overlay is never load-bearing.** The font is bundled into the binary,
  an unreadable one falls back rather than failing, and an encode that fails
  with a caption is retried without one. A print takes hours and cannot be
  repeated; a caption is decoration.
- **ffmpeg is a child process per frame.** One RTSPS session per grab, on
  purpose: a held-open stream locks Bambu Studio out of the camera.

## Conventions

- Comments explain *why*, and the existing ones are load-bearing context, so
  match that density rather than stripping or padding them.
- Magic numbers are lint errors (`mnd`). New literals become named constants
  near the top of the file, as elsewhere.
- `cyclop`, `dupl`, `gocritic`, `gosec` and friends are on; see
  `.golangci.yml`. It is a copy of `openserbia/go-template`'s config, so
  service-specific rules go in an overrides block at the bottom, not inline.
- Tests are stdlib `testing`, table-free, one behaviour per named test, using
  `t.TempDir()` for staging. No test framework, no mocking library.
- Errors wrap with `%w` and are inspected with `errors.As`/`errors.Is`
  (`errorlint` enforces it).

## Deployment shape

CI runs on a self-hosted arm64 Pi runner. The image is built there and tagged
`bambu-timelapse:latest` **locally**, and Watchtower recreates the container
when that tag moves; the deployment never pulls. Tagged `v*` pushes do two
more things: goreleaser cuts a GitHub Release of cross-compiled tarballs
(`.goreleaser.yaml`, run through `task release`), and the same image is pushed
to `ghcr.io/openserbia/bambu-timelapse` as arm64 only, for pulling somewhere
that is not this Pi.

`task dist` is the local dry run of the release build. Version, commit and
date reach the binary through goreleaser's ldflags into `main`, which is what
`bambu-timelapse version` prints; a build made any other way says `dev`.
