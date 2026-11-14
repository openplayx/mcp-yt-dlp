# syntax=docker/dockerfile:1
# Locked dependencies from uv.lock (yt-dlp, yt-dlp-ejs, deno); ffmpeg from
# Debian. Nothing is installed or updated at runtime (yt-dlp -U is not used).
FROM python:3.12-slim-bookworm AS build
RUN pip install --no-cache-dir uv==0.11.23
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/app/.venv
WORKDIR /src
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY README.md ./
COPY src ./src
RUN uv sync --frozen --no-dev --no-editable

FROM python:3.12-slim-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /data /downloads && chmod 0777 /data /downloads
COPY --from=build /app/.venv /app/.venv
COPY UPSTREAM.md /usr/share/doc/openplayx-yt-dlp/
# Runs as any uid (compose sets the host user so files belong to it).
ENV PATH=/app/.venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    HOME=/tmp DENO_DIR=/data/deno XDG_CACHE_HOME=/data/cache DENO_NO_UPDATE_CHECK=1
USER 10001
WORKDIR /data
EXPOSE 8800
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8800/healthz', timeout=3)"]
ENTRYPOINT ["openplayx-yt-dlp", "--config", "/config/config.json", "--secrets", "/run/secrets/yt-dlp.json"]
