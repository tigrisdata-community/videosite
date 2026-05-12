# syntax=docker/dockerfile:1
#
# Build:  docker build -f docker/encoder.Dockerfile -t videosite-encoder:dev .
# Run on Vast.ai: this image expects the env vars listed in
# cmd/videosite-encoder/main.go (JOB_ID, VIDEO_ID, AWS_*, WEBHOOK_*, etc.).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/videosite-encoder ./cmd/videosite-encoder

FROM roflcoopter/amd64-cuda-ffmpeg:20260511143422
# ca-certificates is required so the webhook POST can verify the
# videosite server's TLS cert. The base image ships without it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/videosite-encoder /usr/local/bin/videosite-encoder
CMD ["/usr/local/bin/videosite-encoder"]
