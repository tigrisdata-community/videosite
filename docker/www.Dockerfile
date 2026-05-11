FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o /out/videosite \
  ./cmd/videosite

FROM alpine:3 AS run
RUN apk -U add ca-certificates
COPY --from=build /out/videosite /usr/local/bin/videosite
CMD ["/usr/local/bin/videosite"]