# mcp-yt-dlp

English | [简体中文](README.zh-CN.md)

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An [OpenPlayX](https://github.com/openplayx/openplayx) external service that
wraps [yt-dlp](https://github.com/yt-dlp/yt-dlp) as an MCP server: inspect a video URL, then download it to the server in the
background at a chosen quality. The service is a single Go binary that runs
the pinned yt-dlp executable; clients never reach yt-dlp options directly.

- MCP over Streamable HTTP at `POST /mcp` (stateless JSON-RPC, bearer token),
  the same subset as the other OpenPlayX MCP services. OpenPlayX probes `/mcp`
  from the service root, so the App only needs the root URL and the token.
- `/healthz` (liveness) and `/readyz` (download folder writable, yt-dlp, ffmpeg and
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

### Downloaded file transport

Every file in `get_download` / `list_downloads` has `path`, `bytes`, and
`download_path` (for example `/files/0123456789abcdef/0`; indexes are zero-based).
Append `download_path` to the configured service root, preserving any
reverse-proxy prefix. Only completed jobs can be fetched. These endpoints
share the MCP bearer token and require the `read` scope:

- `GET /files/{job_id}/{file_index}` streams the recorded file.
- `HEAD /files/{job_id}/{file_index}` returns its headers without a body.
- `Range: bytes=<offset>-` resumes an interrupted transfer; `ETag` / `If-Range`
  allow clients to detect a changed file. Invalid ranges return HTTP 416.

Responses include `Content-Length`, `Accept-Ranges: bytes`, and
`Content-Disposition` with an RFC 5987 UTF-8 filename. Files outside the download
root (including escaping symlinks), missing outputs, and unfinished jobs are
never served. All tokens with `read` can read completed jobs, as with the MCP
job listing. No MCP `resources/read` or base64 JSON payload is used. Clients
must keep requests on the configured service address and must not forward
bearer tokens through redirects.

Presets: `best`, `2160p`, `1080p` (default), `720p`, `480p` (height caps,
merged to mp4 when the codecs allow, otherwise mkv) and `audio` (m4a).
Subtitles are saved as `.srt` next to the video. Clients never pass raw
yt-dlp options.

Not supported on purpose: whole playlists (one download per entry URL), live
streams, cookies/logins, `yt-dlp -U` self-update.

### Download progress

While downloading, `progress.scope: "current_stream"` and the 1-based
`stream_index` identify the current stream (video/audio/subtitles in order of
first observation; the stream count is unknown). `downloaded_bytes`,
`total_bytes` (also returned as `current_total_bytes` for compatibility),
`percent` (0–100) and `eta_seconds` all describe that stream. A stream reaching
100% does not mean the job is complete: check `state` and `phase`.
`overall_downloaded_bytes` separately accumulates all streams for the size limit.

`total_bytes_estimated: true` means clients should display the size as approximate
(e.g. “about”); fragment estimates and the resulting percent/ETA can change.
If the total is unknown or smaller than the bytes already downloaded, total,
percent and ETA are null. ETA is rounded up from remaining bytes / `speed_bps`,
marked `eta_estimated: true`, and is null without a usable speed or during
postprocessing. Completed jobs report overall transferred bytes, an exact total
and 100%; output file sizes remain in `files` and may differ after merging.
The job's top-level `estimated_bytes` is only the initial probe estimate and
must not be used to calculate download progress.

## Limits and safety

- Per job size cap (`max_task_bytes`), checked against the estimate before
  queueing and against the bytes actually written while downloading.
- Free space reserve (`min_free_bytes`), checked before queueing and every
  2 s during a download.
- Only public http(s) URLs (no credentials, no private/loopback addresses
  unless `allow_private_addresses`). yt-dlp itself follows redirects and
  fetches further URLs; set `allow_generic_extractor: false` or
  `allowed_extractors` to limit it to known sites.
- Jobs are not resumed after a restart: queued/running ones become `interrupted` and
  their partial files are removed.

## Quick start

Requirements: Docker. For local development: Go 1.26, plus yt-dlp, ffmpeg
and deno on `PATH` to run the service itself.

1. Create a token and the configuration:

   ```bash
   cd deploy
   cp config.example.json config.json
   cp secrets.example.json secrets.json
   TOKEN=$(openssl rand -hex 32)
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
scripts/check.sh        # gofmt, go vet, tests (fake engine, no network)
YTDLP_BIN=$(command -v yt-dlp) scripts/check.sh   # also a real yt-dlp against a local server
go run ./src/cmd/mcp-yt-dlp --config deploy/config.json --secrets deploy/secrets.json
```

`YTDLP_BIN` selects the yt-dlp executable (default: `yt-dlp` on `PATH`).

yt-dlp version and update procedure: [UPSTREAM.md](UPSTREAM.md).

## Migrating from the Python service

Rebuild the image and recreate the container with the existing Compose file,
configuration, tokens, data directory and download directory. The MCP tools,
ports and SQLite job schema stay compatible. Job records and completed
files remain available; queued or running jobs become `interrupted` on
restart and partial files are removed. Submit those downloads again explicitly.

The Go service runs yt-dlp as a subprocess. A local deployment now needs
Go plus the external tools listed above; Docker bundles them. File collection
includes the completed output and its language-tagged sidecars, so another
preset's output is no longer listed or deleted as a sidecar of a new job.

## Contributing

Issues and pull requests are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately as described in [SECURITY.md](SECURITY.md).

## Disclaimer

You are responsible for what you download. Only download content you have the
right to download, and follow the terms of service of the sites involved and
the copyright law that applies to you. This project does not host or
distribute any media, and it is not affiliated with yt-dlp or any site.

## License

[MIT](LICENSE). yt-dlp is a separate program under The Unlicense; ffmpeg and
deno in the Docker image keep their own licenses.
