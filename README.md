# mcp-yt-dlp

OpenPlayX external service that wraps [yt-dlp](https://github.com/yt-dlp/yt-dlp)
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

## Develop

```bash
scripts/check.sh        # ruff + tests (fake engine, no network)
uv run openplayx-yt-dlp --config deploy/config.json --secrets deploy/secrets.json
```

Configuration: `deploy/config.example.json` (no credentials) and
`deploy/secrets.example.json` (token SHA-256 digests, optional proxy URLs).

Deployment on the 211 test host: [docs/deploy-211.md](docs/deploy-211.md).
Upstream version: [UPSTREAM.md](UPSTREAM.md).
