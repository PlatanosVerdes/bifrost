# The module has no requires, so the compile downloads nothing.
FROM golang:1.26-alpine AS build

WORKDIR /src
# The tag this image is built from, logged at start so what is deployed is readable from the logs.
ARG VERSION=local
COPY go.mod ./
COPY *.go ./
COPY web/ ./web/
COPY slate/offair.ts slate/loading.ts ./slate/
# Static binary: the runtime image has no libc to link against.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/bifrost .

# Only what `ffmpeg -c copy` needs to remux MPEG-TS through a pipe, plus a scaled JPEG of one keyframe
# for the page's thumbnails: a few MB instead of Alpine's ~80.
FROM alpine:3.21 AS ffmpeg
ARG FFMPEG_VERSION=7.1.1
RUN apk add --no-cache build-base curl
RUN curl -fsSL https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz | tar -xJ
WORKDIR /ffmpeg-${FFMPEG_VERSION}
RUN ./configure --disable-everything --disable-autodetect --disable-network --disable-doc --disable-debug \
        --disable-ffplay --disable-ffprobe --enable-small --disable-shared --enable-static --extra-ldflags=-static \
        --enable-protocol=pipe --enable-demuxer=mpegts --enable-muxer=mpegts \
        --enable-parser=h264,hevc,mpegvideo,mpeg4video,vc1,aac,aac_latm,ac3,mpegaudio,dca,opus \
        --enable-decoder=h264,hevc,mpeg2video,mpeg4,vc1,aac,aac_latm,ac3,eac3,mp2,mp3,dca,opus \
        --enable-encoder=mjpeg --enable-muxer=mjpeg --enable-filter=scale,format \
    && make -j"$(nproc)" ffmpeg && strip ffmpeg && cp ffmpeg /ffmpeg

# iOS ignores SVG icons on the home screen, so the PNG is rendered from the same favicon.svg, its
# background squared because iOS rounds the corners itself.
FROM alpine:3.21 AS icon
RUN apk add --no-cache rsvg-convert
COPY web/favicon.svg /icon.svg
RUN echo 'svg > rect:first-of-type { rx: 0; ry: 0 }' > /square.css \
    && rsvg-convert --width 180 --height 180 --stylesheet /square.css /icon.svg -o /apple-touch-icon.png

FROM alpine:3.21

# The list comes from IPFS gateways and the guide from GitHub, both over HTTPS.
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/bifrost /usr/local/bin/bifrost
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY --from=icon /apple-touch-icon.png /usr/local/share/bifrost/apple-touch-icon.png

# Owned by the user below so fresh named volumes are writable; bind mounts need the same owner on
# the host, or `user:` set to the host folder's owner.
RUN mkdir -p /data /state && chown 65532:65532 /data /state

# Jellyfin's playlist and the cut-down guide.xml go to /data; run counters and the full guide to /state.
VOLUME ["/data", "/state"]
EXPOSE 6879

# wget is in busybox, so the check needs nothing installed.
HEALTHCHECK --interval=60s --timeout=10s --start-period=120s --retries=3 \
    CMD wget -qO- http://127.0.0.1:6879/healthz >/dev/null || exit 1

USER 65532:65532
ENTRYPOINT ["bifrost"]
