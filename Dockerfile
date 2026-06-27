# syntax=docker/dockerfile:1.7

# --- build stage --------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags='-s -w' -o /out/compactor ./cmd/compactor
RUN go test ./...

# --- runtime stage ------------------------------------------------------------
FROM alpine:3.21 AS runtime
ARG TARGETARCH=amd64

# Compression toolchain. alpine [community] is enabled by default in the image.
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        jpegoptim \
        oxipng \
        libwebp-tools \
        gifsicle \
        ffmpeg \
    && update-ca-certificates

# Install supercronic (container-native cron) for the Docker Compose deployment.
# The K8s CronJob doesn't use it; the ENTRYPOINT stays as the compactor binary.
# Verify the checksum in production: see https://github.com/aptible/supercronic/releases
RUN case "$TARGETARCH" in \
      amd64) SC_ARCH=amd64 ;; \
      arm64) SC_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH=$TARGETARCH" && exit 1 ;; \
    esac && \
    wget -qO /usr/local/bin/supercronic \
      "https://github.com/aptible/supercronic/releases/download/v0.2.30/supercronic-linux-${SC_ARCH}" && \
    chmod +x /usr/local/bin/supercronic

COPY --from=build /out/compactor /usr/local/bin/compactor

# tmpfs inherits the mount-point mode at runtime. Ensure /tmp is world-writable
# so the non-root compactor user can create scratch files.
RUN chmod 1777 /tmp

# Run as a non-root user. The compactor never needs to write to the local fs
# except for /tmp scratch files (the binaries invoked below write there too).
RUN addgroup -S -g 65532 compactor && \
    adduser -S -G compactor -u 65532 -h /tmp compactor
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/compactor"]