# bambu-timelapse

Layer-synced timelapses of Bambu Lab prints, captured from the printer's own
chamber camera and posted to Telegram when the print finishes.

[![A print recorded by this service, at double speed](docs/assets/preview.webp)](docs/assets/example.mp4)

One print, start to finish, at double speed: the slicer's plate render, a frame
per layer, then the finished part. The counter bottom left is the layer each
frame was taken on. [The full-rate MP4](docs/assets/example.mp4) is in
`docs/assets/`.

No cloud, and none of the printer's storage. Telemetry comes off MQTT, frames
off the RTSPS live view, both authenticated with the printer's LAN access code.

## Why not the printer's built-in timelapse

The built-in one records to the SD card or internal store, has to be enabled
per print in the slicer, and has to be fetched afterwards over FTPS. That
means an SD retention policy and a client that can resume a TLS session, since
the printer's vsFTPd requires it. Capturing from the camera needs none of that
and works on every print automatically, including on a printer with no storage
fitted at all.

The trade is the toolhead. Bambu's own "smooth" mode parks it out of frame each
layer using G-code, which this service will not inject, so frames show the head
wherever the layer ended. What it does preserve is layer *sync*, the property
that makes a timelapse watchable, because the trigger is `layer_num` changing
in the telemetry, not a wall clock.

## How it works

```
MQTT device/<serial>/report ── layer_num++ ──▶ ffmpeg: one still off RTSPS
                            └─ RUNNING→FINISH ─▶ encode ─▶ POST to the media
                               API ─▶ delete staging on HTTP 200
```

Frames are numbered sequentially, never by layer: a dropped frame must not
leave a gap, because ffmpeg's `image2` demuxer stops at the first missing
index.

## Surviving restarts

Everything that would otherwise live only in memory goes to
`staging/job-<task_id>/state.json` after every frame: the frame counter, the
true start time, the running temperature statistics. On startup the service
asks the printer for a full snapshot and reconciles:

| Printer says | Action |
| --- | --- |
| `RUNNING`, same `task_id` | Resume: keep the counter and the original start time |
| `RUNNING`, different `task_id` | The old job ended while we were down, so post what was captured |
| `FINISH` / `FAILED` / `IDLE` | Same: post if there are enough frames, else discard |
| Anything in `failed/` | Retry the upload |

Job identity is `task_id`, falling back to `lan_task_id`: a cloud-dispatched
print populates the first and zeroes the second, a LAN-dispatched print does
the reverse. Reading only one silently breaks resume for half of all prints.

Reports are deltas, so the merged view outlives the job it describes. When the
task id changes, the job-scoped fields are dropped rather than carried
over: `layer_num`, `total_layer_num`, `mc_percent`, the title. Without that,
a 13-layer print following a 60-layer one inherits layer 60, no layer ever
looks new, and the print is captured exactly once. Device-scoped fields
(temperatures, the AMS, the camera) survive: the printer sends those rarely
and dropping them would blind the service until the next full snapshot.

A capture that begins after layer 1 sets `partial`, and the caption says so.
Reporting elapsed-since-we-started as the print duration would be a claim the
video itself contradicts.

## Printer setup

Local MQTT and FTPS are available without Developer Mode on current firmware,
but the camera is not. Enable **LAN Only Liveview** on the printer
(Settings → LAN Mode); it does not require LAN-only mode, so cloud and the
Handy app keep working and the access code does not rotate.

Verify without guessing: `rtsp_url` in the MQTT `ipcam` section changes from
the literal string `"disable"` to an `rtsps://…:322/…` URL, and port 322 opens.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `PRINTER_HOST` | required | Printer IP |
| `PRINTER_SERIAL` | required | Serial; the MQTT topic key |
| `PRINTER_ACCESS_CODE` | required | LAN access code (MQTT, RTSPS) |
| `PRINTER_NAME` | `printer` | Alias used in the posted filename |
| `MEDIA_API_URL` | required to post | Endpoint that accepts a multipart video |
| `MEDIA_API_TOKEN` | required to post | Bearer token for it |
| `MEDIA_API_FIELDS` | unset | JSON object of extra form fields, posted verbatim |
| `TIMELAPSE_FPS` | `20` | Playback rate |
| `CAPTURE_DELAY` | `0` | Seconds to wait after a layer change before grabbing |
| `CROP` | unset | ffmpeg crop `w:h:x:y` applied to video and cover |
| `OVERLAY` | `true` | Burn the printer, job and layer counter into the footage |
| `OVERLAY_FONT` | unset | Font to draw with; empty uses the one inside the binary |
| `FFMPEG_BIN` | `ffmpeg` | Encoder binary; an absolute path when PATH cannot be relied on |
| `FFPROBE_BIN` | `ffprobe` | Prober binary, same |
| `INTRO_HOLD` | `2` | Seconds the plate render is held before the timelapse |
| `TAIL_HOLD` | `2` | Seconds the finished print is held after it |
| `PREVIEW_TIMEOUT` | `20` | How long to wait for the plate render over FTPS |
| `MIN_FRAMES` | `30` | Below this the job is discarded, not posted |
| `FINAL_FRAME_DELAY` | `45` | Seconds to wait before the cover shot |
| `MIN_FREE_MB` | `2048` | Refuse to capture below this |
| `FAILED_TTL_DAYS` | `7` | How long parked jobs are kept |
| `LISTEN_ADDR` | `:8092` | `/metrics` and `/healthz` |

### Where the video goes

The service knows one thing about its destination: it `POST`s
`multipart/form-data` with a `video` part (last), an optional `thumbnail`, and
a `caption`. Anything the *consumer* needs to route the result is
configuration, not code:

```sh
MEDIA_API_FIELDS='{"chat_id":"-1001234567890","topic_id":"907","silent":"true"}'
```

Those fields are copied into the form untouched. Keeping them opaque is what
stops a timelapse recorder from turning into a client for whichever chat system
happens to be on the other end; swapping that consumer changes a JSON string,
not this repository. The fields the service owns come after the pass-through
ones (`caption`, `filename`, `duration`, `width`, `height`, `no_audio`), so
configuration cannot misdescribe the file being uploaded.

There is no "media API" product to sign up for. `MEDIA_API_URL` is *your*
endpoint, and what it does with the upload is your business. Forward the file
to a Telegram bot, a Slack `files.upload`, a Discord webhook. Mail it, drop it
in S3 or a NAS folder, write a row and serve it from a gallery. The service
neither knows nor cares. Its whole contract: accept a multipart POST, verify
`Authorization: Bearer <MEDIA_API_TOKEN>` if you care to, answer `200` with

```json
{"data": {"message_ids": [1]}}
```

`message_ids` is a list of integers that gets logged and otherwise ignored, so
an endpoint with nothing to identify can return an empty list. Your status code
does matter. The uploader parks the job on a `4xx` and retries a `5xx`, so say
`503` when your downstream is down and the video should come back, `400` when
it never will.

#### Seeing the request before you write that endpoint

`https://webhook.site` records whatever you post to it. That is enough to read
the multipart body, and to decide how to parse it, before you write a receiver.
Open it, copy the "Your unique URL", and point the service at it:

```sh
MEDIA_API_URL='https://webhook.site/00000000-0000-0000-0000-000000000000'
MEDIA_API_TOKEN='not-a-real-token'
MEDIA_API_FIELDS='{"chat_id":"-1001234567890","topic_id":"907"}'
```

Any other request bin works the same way, and so does `nc -l 8080` or a
ten-line handler on your laptop. The recorded request shows the body in the
order the uploader writes it: the `MEDIA_API_FIELDS` entries (sorted by name),
then `caption`, `filename`, `duration`, `width`, `height`, `no_audio`, then the
optional `thumbnail`, and the `video` part last. `MEDIA_API_TOKEN` arrives as
`Authorization: Bearer …`. Use a placeholder there, because a public bin keeps
everything it receives, and it truncates large bodies. What you get is the
field names and their order, not the file.

Set the bin's response before the first upload, under Edit on the webhook.site
page: status `200`, content type `application/json`, and the body above. Its
default response is plain text, which the uploader cannot parse and treats as
terminal, so the job is parked *after* a successful POST. The request is still
recorded, which is usually all you wanted from it.

Only the daemon posts. `record` is local mode by definition and will not drive
an endpoint however you flag it, so a real request means running the daemon
over a print. Pick a short one.

Use an operator alias for `PRINTER_NAME`, never the serial: the serial is the
MQTT topic key and the cloud-binding identifier, and the filename is published
to a channel.

### Improving the picture

Two knobs address the toolhead, which Bambu's own mode parks with G-code this
service will not inject.

`CAPTURE_DELAY` shifts *when* the frame is taken. At the instant `layer_num`
increments the head is mid-Z-hop and usually dead centre, the worst frame
available. One or two seconds later it has moved on. Costs nothing; layer
times here run 20–55s, and a grab takes about 2.

`CROP` removes the gantry outright, since it occupies a fixed band at the top
of frame. `CROP=1920:820:0:260` keeps everything below it. Applied at encode
time, not capture time, so the frames on disk stay whole and a crop can be
retuned without reprinting. The cover is cropped to match. Validated at
startup, because the encode runs once at the *end* of a print and a typo would
otherwise surface with every frame captured and nothing to show.

### The overlay and the held ends

The video runs: the slicer's plate render for `INTRO_HOLD`, the footage, then
the finished print for `TAIL_HOLD`. What it was meant to be, how it was made,
what it became. Both ends are padding at encode time; neither costs a frame or
a capture, and each is skipped rather than faked when its image is missing. A
run with no finished shot ends by holding the last captured frame instead.

The render is fetched from the printer over FTPS **while the job is printing**,
because that is the only time it is there: a cloud print's 3mf is downloaded to
the internal store for the duration and deleted afterwards. The printer's
vsFTPd wants implicit TLS on 990, wants the data connection to resume the
control connection's TLS session, and starts that handshake only after the
transfer command. `internal/ftps` is those three things and nothing else.
`bambu-timelapse debug` prints what the store currently holds; on an idle
printer the honest answer is "empty".

Burned into the footage are two lines, bottom left: the printer alias and job
name, and under it the layer. The counter is driven by the layer each frame
was *captured* on, recorded in `state.json` alongside the frame count, not by
the frame's position in the sequence. A grab skipped because the previous one
was still running leaves no frame, so counting frames would drift a little
further from the truth with every layer the camera missed. A job resumed from
a state file written before that list existed gets the title line only rather
than a counter that is merely plausible.

The layer text is delivered to ffmpeg as a `sendcmd` script written next to
the frames (`overlay.cmds`), which means a parked job can be re-encoded by
hand with exactly the overlay it would have had. Text taken from the printer
is stripped of anything the filtergraph, the filter option list or drawtext's
own `%{}` expansion would read as syntax: a caption is decoration, and a
filtergraph that fails to parse loses the whole video.

The font travels inside the binary (Go Regular, covering Latin, Latin
Extended, Greek and Cyrillic), materialised into the staging root at startup.
Captioning therefore depends on nothing the host has installed, which is the
point. The first version of this took the font from a distribution package,
and a binary run anywhere that package was not refused to boot over a caption:
the release tarball, a dev machine, a container recreated from an older
image.

Nothing about the overlay is fatal now. `OVERLAY_FONT` points at a different
font if you want one and falls back to the bundled one when it cannot be
read; a font that cannot be written at all logs a warning and turns captions
off for the run; and an encode that fails *with* a caption is retried without
it, because the footage is a print that took hours and the caption is
decoration. `OVERLAY=false` skips the whole thing.

## Preflight

The service asks the host what it can do before it captures anything, and
says so:

```
INFO preflight check=ffmpeg   ok=true  detail=/usr/bin/ffmpeg
WARN preflight check=ffprobe  ok=false detail="not on PATH; the posted video will carry no dimensions"
INFO preflight check=staging  ok=true  detail=/staging
INFO preflight check=overlay  ok=true  detail=/staging/overlay-font.ttf
INFO preflight check=printer  ok=true  detail=192.168.1.50:8883
INFO preflight check=access   ok=true  detail="access code accepted"
WARN preflight check=liveview ok=false detail="322 closed: connection refused; enable LAN Only Liveview on the printer"
WARN preflight check=camera   ok=false detail="no frame: ..."
```

The last four ask the printer rather than the host, and they are the four
questions a timelapse that never arrived is eventually traced back to: is
anything at `PRINTER_HOST`, does `PRINTER_ACCESS_CODE` work, is LAN Only
Liveview enabled, and does a frame actually come back. None of them is fatal.
The printer is allowed to be off when the service starts, and MQTT reconnects
on its own. But a wrong access code otherwise stays silent until the first
layer change, hours in.

Only what makes the service pointless is fatal: no ffmpeg, or a staging tree
it cannot write to. Everything else degrades and names what was lost: an
ffmpeg built without libfreetype turns captions off rather than failing the
encode, a build without `tpad` or `concat` drops the held ends, a missing
ffprobe costs the dimensions and the intro.

The distinction is the point. A print runs for hours and cannot be repeated,
so refusing to start has to be reserved for the cases where starting would
capture nothing anyway. Every other shortfall has to be visible at boot rather
than discovered at the encode, which happens once, at the end, with every
frame already taken.

`bambu-timelapse debug` runs the printer half of this under `== printer ==`.
It deliberately does not repeat the ffmpeg probe: whether this build can draw
a caption is a question about an encode hours away, not about why the printer
is unreachable now.

## Recording without publishing

```sh
bambu-timelapse record                 # capture, then write the video here
bambu-timelapse record -out /tmp -once # one print, then exit
```

Same capture, same encode, same overlay. The finished video, its cover and
the caption it would have carried go to `-out` instead of being posted.
Needs only `PRINTER_HOST`, `PRINTER_SERIAL` and `PRINTER_ACCESS_CODE`:
checking that a crop frames the plate or that a caption reads right should not
require a chat on the other end, or put a test print in one.

`-once` ends the run after the first print, so a recording made to check
something does not leave a daemon behind. If the output directory cannot be
written, the job is parked in `failed/` rather than lost.

### Without a print at all

```sh
bambu-timelapse record -interval 2s -frames 8 -out /tmp
devbox run -- task record -- -interval 2s -frames 8   # the same, from a checkout
```

Captures on a clock instead of on layer changes, which needs no print and no
telemetry, since an idle printer still serves its camera. It is how the crop,
the caption and the encode get checked in a minute rather than in however long
a job takes, and Ctrl-C encodes what has been captured so far rather than
throwing it away.

The result is deliberately not layer synced and carries no layer counter:
there are no layers, and a caption implying otherwise would be a claim the
video contradicts.

## Configuring a local run

Plain `ffmpeg`/`ffprobe` resolve through PATH, which is what the container
relies on. An IDE run configuration, a cron entry or a unit file each have
their own PATH, and preflight will say so at startup. `FFMPEG_BIN` and
`FFPROBE_BIN` take an absolute path for those.

`.env` is read at startup when present, which is how this runs outside the
container. Compose supplies the environment there, so a missing file is not
an error. Copy `.env.example` to `.env` and fill in the printer; it is
gitignored, because the access code is in it.

## Debugging

```sh
bambu-timelapse debug                      # printer checks, ports, state, storage
bambu-timelapse debug -raw                 # the entire merged report as JSON
bambu-timelapse debug -frame /tmp/f.jpg    # also grab one still
```

Needs only `PRINTER_HOST`, `PRINTER_SERIAL` and `PRINTER_ACCESS_CODE`. Being
made to configure a publish destination before it will tell you why the
printer is unreachable gets the diagnostic order backwards.

It opens with the four checks the daemon makes at startup: printer reachable,
access code accepted, LAN Only Liveview enabled, a frame actually returned.
`-frame` keeps that still rather than discarding it. Then it answers the
questions that otherwise cost a throwaway MQTT client: whether `rtsp_url`
still reads `"disable"`, which `task_id` is live, and whether the printer has
any storage at all.

## Operating

`/healthz` reports unhealthy when telemetry goes quiet for five minutes. That
is the failure worth catching. A process that is up but deaf captures nothing
and otherwise looks perfectly healthy.

`bambu_frames_captured_total` flat while `bambu_print_state{state="RUNNING"}`
is 1 means the camera is failing even though the printer is fine.

## Releases

Pushing a `v*` tag cuts a release: cross-compiled `linux/amd64` and
`linux/arm64` archives with a `SHA256SUMS`, attached to a GitHub Release.

The container image is **not published to a registry**. CI builds it on the
rpi runner and tags it `bambu-timelapse:latest` locally, where the service
runs; Watchtower recreates the container when that tag moves. Compose
therefore uses `pull_policy: never`.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Development

```sh
devbox run -- task ci      # secrets, lint, test, build
devbox run -- task test
devbox run -- task secrets # gitleaks, on its own
devbox run -- task hooks   # install the pre-commit hook; once per clone
```

Two tasks drive the binary against the real printer, both reading `.env`:

```sh
devbox run -- task debug                            # the diagnostic snapshot
devbox run -- task debug  -- -raw                   # its flags go after --
devbox run -- task record -- -interval 2s -frames 8 # a video without a print
devbox run -- task record                           # wait for the next print
```

`record` writes into `build/recordings/`, which is already gitignored, so a
test capture never turns up in a commit.

### The pre-commit hook

`task hooks` points `core.hooksPath` at the tracked `.githooks/` directory.
The hook is then a file you can read in a diff, not a copy sitting stale in
somebody's `.git/hooks`. `core.hooksPath` is per-clone git config, so run this
once after cloning.

`.githooks/pre-commit` runs `task secrets` on every commit. It runs `task lint`
only when the commit touches `*.go`, `go.mod` or `go.sum`, because a README
commit should not pay for govulncheck. Two things then fail the commit: a lint
error, or the formatter (gci, gofumpt) rewriting a file. The second one matters
because that rewrite lands in the working tree and not in the index. Commit
through it and you record the unformatted version, with the fix left behind.
Review, `git add`, commit again.

`git commit --no-verify` skips it for one commit. CI runs the same task, so
that defers the check rather than skipping it.

### Secret scanning

`task secrets` runs gitleaks twice. The working-tree pass catches a credential
on its way into a commit. The history pass catches one already in, and that is
the pass that matters after the fact, because deleting the line does not delete
the blob. It is also why CI checks out with `fetch-depth: 0` instead of the
default single commit.

`.gitleaks.toml` allowlists the build and cache trees, and `.env`, which is
*supposed* to hold a live access code and is gitignored for that reason. It
scans everything else, untracked files included. If a real credential does
reach a commit, the scan is the alarm and not the fix. Rotate the value. For
the printer that means Settings → WLAN on the machine, and it is the only
thing that helps, since the old code is in every clone and in the runner's
cache whatever you rewrite the history into.
