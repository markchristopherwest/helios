# syntax=docker/dockerfile:1
# Multi-arch (amd64/arm64) — plays nice with a mixed Pi/NUC cluster.

FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/helios .

# Comskip isn't packaged in Debian; build from source against ffmpeg libs.
FROM debian:bookworm-slim AS comskip
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates build-essential autoconf automake libtool pkg-config \
      libargtable2-dev libavformat-dev libavcodec-dev libavutil-dev libswscale-dev \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 https://github.com/erikkaashoek/Comskip /Comskip \
    && cd /Comskip && ./autogen.sh && ./configure && make -j"$(nproc)"

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ffmpeg libargtable2-0 ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/helios /usr/local/bin/helios
COPY --from=comskip /Comskip/comskip /usr/local/bin/comskip
VOLUME ["/data", "/media", "/recordings"]
EXPOSE 7979
# UDP tuner discovery needs hostNetwork/host networking to see LAN broadcast;
# otherwise set -hdhr (or Settings) to the tuner IP.
ENTRYPOINT ["/usr/local/bin/helios", "-data", "/data", "-media", "/media", "-recordings", "/recordings"]
