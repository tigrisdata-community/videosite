# syntax=docker/dockerfile:1
#
# Build:  docker build -f docker/encoder.Dockerfile -t videosite-encoder:dev .
# Run on Vast.ai: this image expects the env vars listed in
# cmd/videosite-encoder/main.go (JOB_ID, VIDEO_ID, AWS_*, WEBHOOK_*, etc.).
#
# Multi-stage: ffmpeg is built against nv-codec-headers in a CUDA devel image
# and then copied into a CUDA runtime image. NVENC headers compile in;
# the CUDA driver libs come from /usr/local/nvidia mounted by
# nvidia-container-runtime at run time. Build and runtime share the same
# Ubuntu version so glibc/libstdc++ ABIs line up.

FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o /out/videosite-encoder ./cmd/videosite-encoder

FROM nvidia/cuda:12.8.2-devel-ubuntu22.04 AS ffmpeg-build

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=Etc/UTC
ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig/
ENV PATH=/usr/local/cuda/bin:${PATH}

RUN apt-get update && apt-get -y upgrade \
  && apt-get install -y --no-install-recommends \
  ca-certificates \
  libass-dev \
  libfdk-aac-dev \
  librtmp-dev \
  libssl-dev \
  libvorbis-dev \
  libvpx-dev \
  libx264-dev \
  libx265-dev \
  ocl-icd-opencl-dev \
  build-essential \
  cmake \
  gcc \
  git \
  libtool \
  nasm \
  yasm \
  clang \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone --depth 1 -b sdk/12.2 https://github.com/FFmpeg/nv-codec-headers.git \
  && cd nv-codec-headers && make && make install

RUN git clone --depth 1 https://github.com/FFmpeg/FFmpeg && \
  cd /src/FFmpeg && \
  ./configure \
  --prefix="/app/ffmpeg" \
  --disable-debug \
  --enable-cuvid \
  --enable-ffnvcodec \
  --enable-gpl \
  --enable-libass \
  --enable-libfdk-aac \
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
  --enable-static && \
  make -j"$(nproc)" && make install

FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=Etc/UTC
ENV LD_LIBRARY_PATH=/usr/local/nvidia/lib64
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=video,compute,utility
ENV PATH=/app/ffmpeg/bin:${PATH}

# Runtime shared libs, traced from the ffmpeg --enable-* flags above:
#   libass        -> libass9
#   libfdk-aac    -> libfdk-aac2
#   librtmp       -> librtmp1
#   openssl       -> libssl3
#   libvorbis     -> libvorbis0a + libvorbisenc2
#   libvpx        -> libvpx7
#   libx264       -> libx264-163
#   libx265       -> libx265-199
#   opencl        -> ocl-icd-libopencl1 (ICD loader; vendor ICD comes from
#                    the nvidia-container-runtime driver mount)
RUN apt-get update && apt-get -y upgrade \
  && apt-get install -y --no-install-recommends \
  ca-certificates \
  libass9 \
  libfdk-aac2 \
  librtmp1 \
  libssl3 \
  libvorbis0a \
  libvorbisenc2 \
  libvpx7 \
  libx264-163 \
  libx265-199 \
  ocl-icd-libopencl1 \
  && rm -rf /var/lib/apt/lists/*

COPY --from=ffmpeg-build /app/ffmpeg /app/ffmpeg
RUN ldd /app/ffmpeg/bin/ffmpeg \
  && ffmpeg -version
COPY --from=go-build /out/videosite-encoder /usr/local/bin/videosite-encoder
CMD ["/usr/local/bin/videosite-encoder"]
