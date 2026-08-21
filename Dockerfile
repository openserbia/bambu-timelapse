FROM alpine:3.24.1 AS builder

RUN apk add --no-cache build-base bash curl
RUN curl -fsSL https://get.jetpack.io/devbox | FORCE=1 bash

WORKDIR /app
# devbox files first, so this layer caches until the toolchain pins change.
COPY devbox.json devbox.lock ./
RUN devbox install

COPY . /app
RUN --mount=type=cache,target=/root/.cache/go-build \
    devbox run -- bash -c 'task build'

# Alpine, not distroless: the service shells out to ffmpeg for every frame and
# for the encode, so the runtime genuinely needs a userland. The binary itself
# is static (CGO_ENABLED=0, netgo/osusergo) so nothing else is pulled in.
FROM alpine:3.24.1

RUN apk add --no-cache ffmpeg tini \
    && adduser -D -u 1000 bambu \
    && mkdir -p /staging && chown bambu:bambu /staging

COPY --from=builder /app/build/bambu-timelapse /usr/local/bin/bambu-timelapse

USER bambu
WORKDIR /staging
ENV STAGING_DIR=/staging LISTEN_ADDR=:8092
EXPOSE 8092

# tini reaps the ffmpeg children: one process is spawned per layer, and over a
# 500-layer print an unreaped pile of zombies would exhaust the pids limit.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/bambu-timelapse"]

HEALTHCHECK --interval=60s --timeout=10s --start-period=90s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8092/healthz || exit 1
