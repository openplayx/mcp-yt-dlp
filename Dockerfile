# syntax=docker/dockerfile:1
# Go service plus pinned, checksum-verified runtime tools: the official
# yt-dlp onedir build (bundles yt-dlp-ejs), deno as its JS runtime, and
# ffmpeg from Debian. Nothing is installed or updated at runtime
# (yt-dlp -U is not used). Versions: UPSTREAM.md.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mcp-yt-dlp ./src/cmd/mcp-yt-dlp

FROM debian:bookworm-slim AS ytdlp
ARG TARGETARCH
ARG YTDLP_VERSION=2026.10.26
# SHA2-256SUMS of the release: yt-dlp_linux.zip / yt-dlp_linux_aarch64.zip
# (onedir builds: no self-extraction to /tmp on every run).
ARG YTDLP_SHA256_AMD64=f34053415136e55073dd64ba5326d161ecc23ce0e98c081d8752867b37809450
ARG YTDLP_SHA256_ARM64=74708e707e8fa8177c8347e58152337edf320149cd7b225defd453f7cde47822
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) asset=yt-dlp_linux; sum="$YTDLP_SHA256_AMD64" ;; \
      arm64) asset=yt-dlp_linux_aarch64; sum="$YTDLP_SHA256_ARM64" ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/yt-dlp.zip "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${asset}.zip"; \
    echo "$sum  /tmp/yt-dlp.zip" | sha256sum -c -; \
    mkdir /opt/yt-dlp; unzip -q /tmp/yt-dlp.zip -d /opt/yt-dlp; \
    mv "/opt/yt-dlp/${asset}" /opt/yt-dlp/yt-dlp; chmod -R a+rX /opt/yt-dlp; \
    /opt/yt-dlp/yt-dlp --version

FROM denoland/deno:bin-2.9.8 AS deno

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /data /downloads && chmod 0777 /data /downloads
COPY --from=ytdlp /opt/yt-dlp /opt/yt-dlp
COPY --from=deno /deno /usr/local/bin/deno
COPY --from=build /out/mcp-yt-dlp /usr/local/bin/mcp-yt-dlp
COPY UPSTREAM.md LICENSE /usr/share/doc/openplayx-yt-dlp/
# Runs as any uid (compose sets the host user so files belong to it).
ENV HOME=/tmp DENO_DIR=/data/deno XDG_CACHE_HOME=/data/cache DENO_NO_UPDATE_CHECK=1 \
    YTDLP_BIN=/opt/yt-dlp/yt-dlp PATH=/opt/yt-dlp:$PATH
USER 10001
WORKDIR /data
EXPOSE 8800
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["/usr/local/bin/mcp-yt-dlp", "healthcheck", "http://127.0.0.1:8800/healthz"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/mcp-yt-dlp", "--config", "/config/config.json", "--secrets", "/run/secrets/yt-dlp.json"]
