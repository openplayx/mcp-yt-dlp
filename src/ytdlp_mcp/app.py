"""FastAPI application factory: /healthz, /readyz and /mcp."""

from __future__ import annotations

import logging
import os
import shutil
import time
import uuid
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from . import __version__
from .auth import Authenticator
from .config import Config, Secrets, resolve_proxy
from .engine import Engine, YtDlpEngine
from .errors import ApiError
from .jobs import DiskUsage, JobManager, Store
from .mcp import router as mcp_router
from .netguard import Resolver, system_resolver
from .service import Service

log = logging.getLogger("ytdlp_mcp")


def create_app(
    config: Config,
    secrets: Secrets,
    *,
    engine: Engine | None = None,
    resolver: Resolver = system_resolver,
    disk_usage: DiskUsage = shutil.disk_usage,
) -> FastAPI:
    """``engine``/``resolver``/``disk_usage`` are injection points for tests."""
    proxy = resolve_proxy(config, secrets)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        Path(config.storage.cache_dir).mkdir(parents=True, exist_ok=True)
        store = Store(config.storage.database)
        eng = engine or YtDlpEngine(config, proxy)
        jobs = JobManager(config, eng, store, disk_usage=disk_usage)
        app.state.service = Service(config, eng, jobs, resolver)
        app.state.auth = Authenticator(secrets, config.limits.requests_per_minute)
        if not secrets.tokens:
            log.warning("no API tokens configured; every /mcp request is rejected")
        try:
            yield
        finally:
            jobs.shutdown()
            store.close()

    app = FastAPI(
        title="OpenPlayX yt-dlp",
        version=__version__,
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )
    app.include_router(mcp_router)

    @app.get("/healthz", include_in_schema=False)
    async def healthz() -> dict:
        return {"status": "ok", "version": __version__}

    @app.get("/readyz", include_in_schema=False)
    async def readyz(request: Request):
        svc: Service = request.app.state.service
        root = svc.jobs.root
        checks = {
            "download_root_writable": os.access(root, os.W_OK),
            "ffmpeg": shutil.which("ffmpeg") is not None,
            "js_runtime": shutil.which("deno") is not None,
        }
        ok = all(checks.values())
        return JSONResponse(
            {"status": "ready" if ok else "not_ready", "checks": checks},
            status_code=200 if ok else 503,
        )

    @app.middleware("http")
    async def request_log(request: Request, call_next):
        rid = uuid.uuid4().hex[:12]
        started = time.monotonic()
        response = await call_next(request)
        # Never log bodies (URLs, tokens); tool name and error code only.
        if request.url.path not in ("/healthz", "/readyz"):
            log.info(
                "request id=%s method=%s path=%s status=%d ms=%d principal=%s "
                "tool=%s error=%s",
                rid,
                request.method,
                request.url.path,
                response.status_code,
                (time.monotonic() - started) * 1000,
                getattr(request.state, "principal", "-"),
                getattr(request.state, "tool", "-"),
                getattr(request.state, "error_code", "-"),
            )
        return response

    @app.exception_handler(ApiError)
    async def api_error(request: Request, exc: ApiError):
        request.state.error_code = exc.code
        return JSONResponse(exc.body(), status_code=exc.status, headers=exc.headers)

    return app
