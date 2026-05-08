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

FROM jrottenberg/ffmpeg:nvidia
COPY --from=build /out/videosite-encoder /usr/local/bin/videosite-encoder
ENTRYPOINT ["/usr/local/bin/videosite-encoder"]
