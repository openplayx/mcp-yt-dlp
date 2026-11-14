"""Entry point: ``openplayx-yt-dlp --config ... --secrets ...``."""

from __future__ import annotations

import argparse
import logging
import os
import sys


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="openplayx-yt-dlp")
    parser.add_argument("--config", default=os.environ.get("YTDLP_MCP_CONFIG"))
    parser.add_argument("--secrets", default=os.environ.get("YTDLP_MCP_SECRETS"))
    parser.add_argument("--host", help="Overrides server.host")
    parser.add_argument("--port", type=int, help="Overrides server.port")
    args = parser.parse_args(argv)

    from .app import create_app
    from .config import load_config, load_secrets

    config = load_config(args.config)
    secrets = load_secrets(args.secrets)
    logging.basicConfig(
        level=config.server.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        stream=sys.stderr,
    )

    import uvicorn

    uvicorn.run(
        create_app(config, secrets),
        host=args.host or config.server.host,
        port=args.port or config.server.port,
        proxy_headers=False,
        server_header=False,
        log_config=None,
        access_log=False,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
