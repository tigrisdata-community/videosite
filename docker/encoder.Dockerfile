# syntax=docker/dockerfile:1
#
# Build:  docker build -f docker/encoder.Dockerfile -t videosite-encoder:dev .
# Run on Vast.ai: this image expects the env vars listed in
# cmd/videosite-encoder/main.go (JOB_ID, VIDEO_ID, AWS_*, WEBHOOK_*, etc.).
#
# We build ffmpeg from source against nv-codec-headers so NVENC works on
# any CUDA-12-class driver Vast hands us. The cuda devel image is also
# the runtime image — trying to copy just ffmpeg into a slim runtime
# drags in a long tail of CUDA shared libs and isn't worth the bytes.

FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/videosite-encoder ./cmd/videosite-encoder

FROM nvidia/cuda:12.8.2-devel-ubuntu24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=Etc/UTC
ENV LD_LIBRARY_PATH=/usr/local/nvidia/lib64
ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig/
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=video,compute,utility
ENV PATH=/usr/local/cuda/bin:${PATH}

RUN apt-get update && apt-get -y upgrade \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      libass-dev \
      libfdk-aac-dev \
      librtmp-dev \
      libssl-dev \
      libvdpau1 \
      libvorbis-dev \
      libvpx-dev \
      libx264-dev \
      libx265-dev \
      build-essential \
      cmake \
      gcc \
      git \
      libc6 \
      libc6-dev \
      libtool \
      nasm \
      nvidia-cuda-toolkit \
      yasm \
      clang \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone --depth 1 -b sdk/12.2 https://github.com/FFmpeg/nv-codec-headers.git \
 && cd nv-codec-headers && make && make install

RUN git clone --depth 1 https://github.com/FFmpeg/FFmpeg && \
  cd /src/FFmpeg && \
  ./configure \
    --prefix="/usr/local" \
    --disable-debug \
    --enable-cuda \
    --enable-cuda-llvm \
    --enable-cuvid \
    --enable-ffnvcodec \
    --enable-gpl \
    --enable-libass \
    --enable-libfdk-aac \
    --enable-libnpp \
    --enable-librtmp \
    --enable-libvorbis \
    --enable-libvpx \
    --enable-libx264 \
    --enable-libx265 \
    --enable-nonfree \
    --enable-nvenc \
    --enable-opencl \
    --enable-openssl \
    --enable-pic \
    --enable-static \
    --extra-cflags="-I/usr/local/nvidia/include/" \
    --extra-ldflags="-L/usr/local/nvidia/lib64/" && \
  make -j"$(nproc)" && make install && \
  cd / && rm -rf /src

COPY --from=go-build /out/videosite-encoder /usr/local/bin/videosite-encoder
CMD ["/usr/local/bin/videosite-encoder"]
