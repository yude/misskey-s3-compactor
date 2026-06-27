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

COPY --from=build /out/compactor /usr/local/bin/compactor

# Run as a non-root user. The compactor never needs to write to the local fs
# except for /tmp scratch files (the binaries invoked below write there too).
RUN addgroup -S -g 65532 compactor && \
    adduser -S -G compactor -u 65532 -h /tmp compactor
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/compactor"]