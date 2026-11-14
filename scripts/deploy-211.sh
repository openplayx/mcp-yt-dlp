#!/usr/bin/env bash
# Build and (re)deploy a committed revision to the 211 test host.
#
#   scripts/deploy-211.sh [REF]          # default HEAD; must be committed
#   IMAGE_TAG=<tag> scripts/deploy-211.sh --rollback
#
# Independent Compose project "openplayx-yt-dlp" under ~/openplayx-yt-dlp on
# the host (releases/<tag>, deploy/, data/, downloads/); never touches other
# projects and never runs a global docker prune. The container runs as the
# SSH user so downloads belong to it. The API token is generated on the host,
# stored only as its SHA-256 in secrets.json, and the plain token is left in
# deploy/app-token.txt (0600); it is never printed.
set -euo pipefail

HOST="${DEPLOY_HOST:-zyl@192.168.1.211}"
BIND="${BIND:-192.168.1.211}"
PORT="${PORT:-18800}"
REMOTE_DIR="${REMOTE_DIR:-openplayx-yt-dlp}"
PROJECT="openplayx-yt-dlp"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

remote() { ssh -o BatchMode=yes "$HOST" "$@"; }

if [[ "${1:-}" == "--rollback" ]]; then
  : "${IMAGE_TAG:?IMAGE_TAG=<previous tag> is required for rollback}"
  TAG="$IMAGE_TAG"
else
  REF="${1:-HEAD}"
  cd "$ROOT"
  if [[ -n "$(git status --porcelain --untracked-files=no)" && "$REF" == "HEAD" ]]; then
    echo "working tree has uncommitted changes; commit first" >&2
    exit 1
  fi
  COMMIT="$(git rev-parse --verify "$REF^{commit}")"
  TAG="${COMMIT:0:12}"
  ARCHIVE="$(mktemp -t yt-dlp.XXXXXX).tar.gz"
  trap 'rm -f "$ARCHIVE"' EXIT
  git archive --format=tar.gz -o "$ARCHIVE" "$COMMIT"
  remote "mkdir -p ~/$REMOTE_DIR/releases/$TAG ~/$REMOTE_DIR/deploy ~/$REMOTE_DIR/data ~/$REMOTE_DIR/downloads"
  scp -q "$ARCHIVE" "$HOST:$REMOTE_DIR/releases/$TAG/source.tar.gz"
  remote "cd ~/$REMOTE_DIR/releases/$TAG && tar xzf source.tar.gz && rm source.tar.gz \
    && sudo docker build -q --label org.opencontainers.image.revision=$COMMIT \
       -t openplayx/yt-dlp:$TAG . \
    && cp deploy/compose.yaml ~/$REMOTE_DIR/deploy/compose.yaml"
fi

# First deployment: config from the example, token generated on the host.
remote "set -e; cd ~/$REMOTE_DIR/deploy
  if [ ! -f config.json ]; then
    cp ../releases/$TAG/deploy/config.example.json config.json
  fi
  if [ ! -f secrets.json ]; then
    umask 077
    python3 - <<'PY'
import hashlib, json, secrets
token = secrets.token_urlsafe(32)
open('app-token.txt', 'w').write(token + '\n')
json.dump({'tokens': [{'name': 'openplayx-app',
                       'token_sha256': hashlib.sha256(token.encode()).hexdigest(),
                       'scopes': ['read', 'download']}],
           'proxies': {}}, open('secrets.json', 'w'), indent=2)
PY
    chmod 0400 secrets.json
  fi"

remote "cd ~/$REMOTE_DIR/deploy && sudo env YTDLP_IMAGE=openplayx/yt-dlp:$TAG \
  APP_UID=\$(id -u) APP_GID=\$(id -g) DATA_DIR=\$HOME/$REMOTE_DIR/data \
  DOWNLOAD_ROOT=\$HOME/$REMOTE_DIR/downloads BIND=$BIND PORT=$PORT \
  docker compose -p $PROJECT -f compose.yaml up -d --remove-orphans \
  && echo $TAG > current-tag"

for _ in $(seq 1 30); do
  if curl -fsS "http://$BIND:$PORT/readyz" >/dev/null 2>&1; then
    echo "deployed tag=$TAG base_url=http://$BIND:$PORT mcp=http://$BIND:$PORT/mcp"
    remote "sudo docker image inspect --format '{{.Id}}' openplayx/yt-dlp:$TAG"
    exit 0
  fi
  sleep 1
done
echo "service did not become ready; readyz and recent logs:" >&2
curl -sS "http://$BIND:$PORT/readyz" >&2 || true
remote "sudo docker logs --tail 50 ${PROJECT}-yt-dlp-1" >&2
exit 1
