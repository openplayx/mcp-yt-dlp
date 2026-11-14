# Upstream

yt-dlp is used as an unmodified dependency, not copied into this tree.

| | |
|---|---|
| Project | https://github.com/yt-dlp/yt-dlp |
| Version | 2026.10.26 (PyPI `yt-dlp[default,deno]`, exact build locked in `uv.lock`) |
| License | The Unlicense |
| Extras | `yt-dlp-ejs` (YouTube JS challenge solver), `deno` (JS runtime) |
| ffmpeg | Debian bookworm package in the image |

Updating: change the pin in `pyproject.toml`, run `uv lock`, run
`scripts/check.sh`, redeploy and repeat the smoke test in
`docs/deploy-211.md`. Sites change often, so expect to bump yt-dlp regularly;
the image never self-updates.
