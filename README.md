# mcp-yt-dlp

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An [OpenPlayX](https://github.com/openplayx/openplayx) external service that wraps [yt-dlp](https://github.com/yt-dlp/yt-dlp)
as an MCP server: inspect a video URL, then download it to the server in the
background at a chosen quality.

- MCP over Streamable HTTP at `POST /mcp` (stateless JSON-RPC, bearer token),
  the same subset as the other OpenPlayX MCP services. OpenPlayX probes `/mcp`
  from the service root, so the App only needs the root URL and the token.
- `/healthz` (liveness) and `/readyz` (download folder writable, ffmpeg and
  the JS runtime present) need no token.

## Tools

| Tool | Scope | Read-only | What it does |
|---|---|---|---|
| `get_status` | read | yes | Free space, limits, presets, job counts |
| `get_media_info` | read | yes | Title, duration, resolutions, subtitle languages, estimated size per preset; playlist entries for a playlist URL |
| `start_download` | download | no | Queues one video (`url`, `preset`, `subtitles`); checks size, duration and free space first; returns the job |
| `list_downloads` | read | yes | Recent jobs, filter by state |
| `get_download` | read | yes | Progress, resulting files, error of one job |
| `cancel_download` | download | no | Stops a queued/running job, discards partial files |
| `remove_download` | download | no (destructive) | Forgets a finished job; `delete_files: true` also deletes its files |

OpenPlayX asks the user to confirm every tool that is not read-only.

Presets: `best`, `2160p`, `1080p` (default), `720p`, `480p` (height caps,
merged to mp4 when the codecs allow, otherwise mkv) and `audio` (m4a).
Subtitles are saved as `.srt` next to the video. Clients never pass raw
yt-dlp options.

Not supported on purpose: whole playlists (one download per entry URL), live
streams, cookies/logins, `yt-dlp -U` self-update.

## Limits and safety

- Per job size cap (`max_task_bytes`), checked against the estimate before
  queueing and against the bytes actually written while downloading.
- Free space reserve (`min_free_bytes`), checked before queueing and every
  2 s during a download.
- Only public http(s) URLs (no credentials, no private/loopback addresses
  unless `allow_private_addresses`). yt-dlp itself follows redirects and
  fetches further URLs; set `allow_generic_extractor: false` or
  `allowed_extractors` to limit it to known sites.
- Jobs do not survive a restart: running ones become `interrupted` and
  their partial files are removed.

## Quick start

Requirements: Docker, or Python 3.12 with [uv](https://docs.astral.sh/uv/)
and ffmpeg for local development.

1. Create a token and the configuration:

   ```bash
   cd deploy
   cp config.example.json config.json
   cp secrets.example.json secrets.json
   TOKEN=$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')
   printf '%s' "$TOKEN" | shasum -a 256   # put the digest into secrets.json → token_sha256
   ```

   Instead of a digest, `"token": "<plain token>"` also works.

2. Build and start:

   ```bash
   docker build -t openplayx/yt-dlp:local ..
   mkdir -p ../data ../downloads
   YTDLP_IMAGE=openplayx/yt-dlp:local APP_UID=$(id -u) APP_GID=$(id -g) \
     DATA_DIR=../data DOWNLOAD_ROOT=../downloads docker compose up -d
   ```

   The service listens on `127.0.0.1:18800` (`BIND` and `PORT` change it);
   downloaded files land in `downloads/`.

3. Check it:

   ```bash
   curl -s http://127.0.0.1:18800/readyz
   curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_status","arguments":{}}}' \
     http://127.0.0.1:18800/mcp
   ```

For anything beyond your own machine or LAN, put an HTTPS reverse proxy in front.

## Configuration

| File | Content |
|---|---|
| `config.json` ([example](deploy/config.example.json)) | listen address, storage paths, limits (`max_task_bytes`, `min_free_bytes`, `max_duration_seconds`, concurrency, rate limit), network policy (`allow_private_addresses`, `allowed_extractors`, `allow_generic_extractor`, `proxy_ref`) |
| `secrets.json` ([example](deploy/secrets.example.json)) | tokens (name, `token` or `token_sha256`, scopes `read` / `download`), optional proxy URLs referenced by `proxy_ref` |

`secrets.json` and `config.json` are git-ignored.

## Using it from an MCP client

Any client that speaks Streamable HTTP with a bearer token works. For example:

```json
{
  "mcpServers": {
    "yt-dlp": {
      "type": "http",
      "url": "http://127.0.0.1:18800/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

In OpenPlayX, add it under **AI → 外部能力** with the service root URL and the token.

## Develop

```bash
uv sync --all-groups
scripts/check.sh        # ruff + tests (fake engine, no network)
uv run openplayx-yt-dlp --config deploy/config.json --secrets deploy/secrets.json
```

yt-dlp version and update procedure: [UPSTREAM.md](UPSTREAM.md).

## Contributing

Issues and pull requests are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately as described in [SECURITY.md](SECURITY.md).

## Disclaimer

You are responsible for what you download. Only download content you have the
right to download, and follow the terms of service of the sites involved and
the copyright law that applies to you. This project does not host or
distribute any media, and it is not affiliated with yt-dlp or any site.

## License

[MIT](LICENSE). yt-dlp is a separate dependency under The Unlicense; ffmpeg in
the Docker image is the Debian package under its own licenses.
