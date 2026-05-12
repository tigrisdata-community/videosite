# syntax=docker/dockerfile:1
#
# Build:  docker build -f docker/encoder.Dockerfile -t videosite-encoder:dev .
# Run on Vast.ai: this image expects the env vars listed in
# cmd/videosite-encoder/main.go (JOB_ID, VIDEO_ID, AWS_*, WEBHOOK_*, etc.).

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/videosite-encoder ./cmd/videosite-encoder

FROM roflcoopter/amd64-cuda-ffmpeg:20260511143422
# Pull the CA bundle from the build stage instead of installing via the
# runtime image's package manager — the CUDA ffmpeg base doesn't have a
# working apt in this tag. /etc/ssl/certs/ca-certificates.crt is one of
# the default paths Go's crypto/x509 looks in.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/videosite-encoder /usr/local/bin/videosite-encoder
CMD ["/usr/local/bin/videosite-encoder"]
