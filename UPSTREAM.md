# Upstream

yt-dlp is used as an unmodified external program, not copied into this tree.
The Go service runs it as a subprocess with options it builds itself.

| | |
|---|---|
| Project | https://github.com/yt-dlp/yt-dlp |
| Version | 2026.10.26, official onedir build `yt-dlp_linux.zip` / `yt-dlp_linux_aarch64.zip` (SHA-256 pinned in the `Dockerfile`) |
| License | The Unlicense |
| Bundled | `yt-dlp-ejs` (YouTube JS challenge solver) is part of the official build |
| JS runtime | deno 2.9.8 (`denoland/deno:bin-2.9.8` image) |
| ffmpeg | Debian bookworm package in the image |

Updating: change `YTDLP_VERSION` and both `YTDLP_SHA256_*` values in the
`Dockerfile` (from the release's `SHA2-256SUMS`), bump the deno image if the
release notes ask for it, run `scripts/check.sh` with `YTDLP_BIN` pointing at
the new binary, rebuild and redeploy, then call `get_media_info` and a small
`start_download` against a real video (see the README quick start). Sites
change often, so expect to bump yt-dlp regularly; the image never
self-updates.
