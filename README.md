# bambu-timelapse

Layer-synced timelapses of Bambu Lab prints, captured from the printer's own
chamber camera and posted to Telegram when the print finishes.

No cloud, and none of the printer's storage. Telemetry comes off MQTT, frames
off the RTSPS live view, both authenticated with the printer's LAN access code.

## Why not the printer's built-in timelapse

The built-in one records to the SD card or internal store, has to be enabled
per print in the slicer, and has to be fetched afterwards over FTPS — which
means an SD retention policy and a client that can resume a TLS session, since
the printer's vsFTPd requires it. Capturing from the camera needs none of that
and works on every print automatically, including on a printer with no storage
fitted at all.

The trade is the toolhead. Bambu's own "smooth" mode parks it out of frame each
layer using G-code, which this service will not inject, so frames show the head
wherever the layer ended. What it does preserve is layer *sync* — the property
that makes a timelapse watchable — because the trigger is `layer_num` changing
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

Everything that would otherwise live only in memory — the frame counter, the
true start time, the running temperature statistics — is written to
`staging/job-<task_id>/state.json` after every frame. On startup the service
asks the printer for a full snapshot and reconciles:

| Printer says | Action |
| --- | --- |
| `RUNNING`, same `task_id` | Resume: keep the counter and the original start time |
| `RUNNING`, different `task_id` | The old job ended while we were down — post what was captured |
| `FINISH` / `FAILED` / `IDLE` | Same: post if there are enough frames, else discard |
| Anything in `failed/` | Retry the upload |

Job identity is `task_id`, falling back to `lan_task_id`: a cloud-dispatched
print populates the first and zeroes the second, a LAN-dispatched print does
the reverse. Reading only one silently breaks resume for half of all prints.

A capture that begins after layer 1 sets `partial`, and the caption says so —
reporting elapsed-since-we-started as the print duration would be a claim the
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
| `PRINTER_HOST` | — | Printer IP |
| `PRINTER_SERIAL` | — | Serial; the MQTT topic key |
| `PRINTER_ACCESS_CODE` | — | LAN access code (MQTT, RTSPS) |
| `PRINTER_NAME` | `printer` | Alias used in the posted filename |
| `MEDIA_API_URL` | — | `POST /v1/videos` endpoint |
| `MEDIA_API_TOKEN` | — | Bearer token for it |
| `TELEGRAM_CHAT_ID` | — | Target channel |
| `TELEGRAM_TOPIC_ID` | `0` | Forum topic; `0` posts to the channel root |
| `TELEGRAM_SILENT` | `true` | Post without a notification |
| `TIMELAPSE_FPS` | `20` | Playback rate |
| `MIN_FRAMES` | `30` | Below this the job is discarded, not posted |
| `FINAL_FRAME_DELAY` | `45` | Seconds to wait before the cover shot |
| `MIN_FREE_MB` | `2048` | Refuse to capture below this |
| `FAILED_TTL_DAYS` | `7` | How long parked jobs are kept |
| `LISTEN_ADDR` | `:8092` | `/metrics` and `/healthz` |

Use an operator alias for `PRINTER_NAME`, never the serial: the serial is the
MQTT topic key and the cloud-binding identifier, and the filename is published
to a channel.

## Operating

`/healthz` reports unhealthy when telemetry goes quiet for five minutes. That
is the failure worth catching — a process that is up but deaf captures nothing
and otherwise looks perfectly healthy.

`bambu_frames_captured_total` flat while `bambu_print_state{state="RUNNING"}`
is 1 means the camera is failing even though the printer is fine.

## Development

```sh
devbox run -- task ci     # lint, test, build
devbox run -- task test
```
