# 211 test deployment

The target is the LAN test host `192.168.1.211` (x86_64, Docker 29). It is
shared with unrelated services, so this deployment stays inside its own
Compose project and directory.

| | |
|---|---|
| Compose project | `openplayx-yt-dlp` |
| Host directory | `~/openplayx-yt-dlp/`: `releases/<tag>/` sources, `deploy/` config, secrets and compose, `data/` SQLite + caches, `downloads/` files |
| Listen | `192.168.1.211:18800` maps to container port 8800 |
| Runs as | the SSH user (`zyl`), so downloaded files belong to it |
| Image | `openplayx/yt-dlp:<first 12 chars of commit>`, built on the host |

## Deploy

```bash
scripts/deploy-211.sh            # deploys the committed HEAD
scripts/deploy-211.sh <commit>
IMAGE_TAG=<previous tag> scripts/deploy-211.sh --rollback
```

The first deployment copies `config.json` from `deploy/config.example.json`
and generates `secrets.json` with one token (`openplayx-app`, scopes `read`
and `download`). Only the SHA-256 goes into `secrets.json`; the plain token
is in `~/openplayx-yt-dlp/deploy/app-token.txt` (0600). It is never printed
or committed.

The root disk of 211 is nearly full, so the example config keeps a 10 GiB
free-space reserve and caps one download at 4 GiB. Old images and
`releases/<tag>/` directories stay until removed by hand. Never run a global
`docker system prune` on this host.

## OpenPlayX debug seed

`openplayx/debug-seed.json` → `externalServices`:

```json
{ "id": "t211-yt-dlp", "name": "211 yt-dlp",
  "baseUrl": "http://192.168.1.211:18800", "token": "${YTDLP_TOKEN}" }
```

with `YTDLP_TOKEN=<contents of app-token.txt>` in `openplayx/.env`, then
`make seed && make seed-check`.

## Smoke test

```bash
T=$(ssh zyl@192.168.1.211 cat openplayx-yt-dlp/deploy/app-token.txt)
mcp() { curl -s -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d "$1" http://192.168.1.211:18800/mcp; }
mcp '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_status","arguments":{}}}'
mcp '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"start_download","arguments":{"url":"https://www.youtube.com/watch?v=jNQXAC9IVRw","preset":"480p"}}}'
```

then `get_download` with the returned `job_id` until `completed`.
