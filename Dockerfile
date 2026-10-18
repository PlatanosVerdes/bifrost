# The module has no requires, so the compile downloads nothing.
FROM golang:1.26-alpine AS build

WORKDIR /src
# The tag this image is built from, logged at start so what is deployed is readable from the logs.
ARG VERSION=local
COPY go.mod ./
COPY *.go ./
COPY web/ ./web/
# Static binary: the runtime image has no libc to link against.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/bifrost .

FROM alpine:3.21

# The list comes from IPFS gateways and the guide from GitHub, both over HTTPS.
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/bifrost /usr/local/bin/bifrost

# Jellyfin's playlist and the cut-down guide.xml go to /data; run counters and the full guide to /state.
VOLUME ["/data", "/state"]
EXPOSE 6879

# wget is in busybox, so the check needs nothing installed.
HEALTHCHECK --interval=60s --timeout=10s --start-period=120s --retries=3 \
    CMD wget -qO- http://127.0.0.1:6879/healthz >/dev/null || exit 1

ENTRYPOINT ["bifrost"]
