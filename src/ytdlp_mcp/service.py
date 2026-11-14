"""Business operations behind the MCP tools."""

from __future__ import annotations

import asyncio
import logging

from . import __version__, errors
from .config import Config
from .engine import Engine, EngineError
from .jobs import JobManager
from .media import PRESETS, estimate_bytes, summarize
from .netguard import Resolver, check_url

log = logging.getLogger("ytdlp_mcp")

_ENGINE_ERRORS = {
    "unsupported_url": errors.unsupported_url,
    "extraction_failed": errors.extraction_failed,
}


class Service:
    def __init__(
        self, config: Config, engine: Engine, jobs: JobManager, resolver: Resolver
    ) -> None:
        self.config = config
        self.engine = engine
        self.jobs = jobs
        self._resolver = resolver
        self._probe_slots = asyncio.Semaphore(4)

    def _check(self, url: str) -> str:
        return check_url(
            url,
            allow_private=self.config.network.allow_private_addresses,
            resolver=self._resolver,
        )

    async def _probe(self, url: str) -> dict:
        try:
            async with self._probe_slots:
                return await asyncio.wait_for(
                    asyncio.to_thread(self.engine.probe, url),
                    timeout=self.config.limits.info_timeout_seconds,
                )
        except TimeoutError as exc:
            raise errors.timeout() from exc
        except EngineError as exc:
            if exc.code == "upstream_timeout":
                raise errors.timeout() from exc
            make = _ENGINE_ERRORS.get(exc.code, errors.extraction_failed)
            raise make(exc.message) from exc

    def status(self) -> dict:
        lim = self.config.limits
        return {
            "service": "openplayx-yt-dlp",
            "version": __version__,
            "yt_dlp_version": self.engine.version,
            "free_bytes": self.jobs.free_bytes(),
            "reserve_bytes": lim.min_free_bytes,
            "max_task_bytes": lim.max_task_bytes,
            "max_duration_seconds": lim.max_duration_seconds,
            "max_concurrent_downloads": lim.max_concurrent_downloads,
            "jobs": self.jobs.counts(),
            "presets": [{"preset": p.key, "label": p.label} for p in PRESETS.values()],
            "generic_sites_allowed": self.config.network.allow_generic_extractor,
        }

    async def info(self, url: str) -> dict:
        url = self._check(url)
        info = await self._probe(url)
        return summarize(info, max_task_bytes=self.config.limits.max_task_bytes)

    async def start(
        self, principal: str, url: str, preset: str, subtitles: list[str]
    ) -> dict:
        lim = self.config.limits
        url = self._check(url)
        if len(subtitles) > lim.max_subtitle_languages:
            raise errors.invalid_request(
                f"At most {lim.max_subtitle_languages} subtitle languages."
            )
        existing = self.jobs.find_active(url, preset)
        if existing is not None:
            return {"created": False, "job": existing.view()}
        free = self.jobs.free_bytes()
        if free < lim.min_free_bytes:
            raise errors.insufficient_space(free, lim.min_free_bytes)

        info = await self._probe(url)
        kind = info.get("_type")
        if kind in ("playlist", "multi_video"):
            entries = info.get("entries") or []
            raise errors.playlist_not_supported(
                info.get("playlist_count") or len(entries) or None
            )
        if info.get("is_live") or info.get("live_status") in ("is_live", "is_upcoming"):
            raise errors.live_not_supported()
        duration = info.get("duration")
        if duration and duration > lim.max_duration_seconds:
            raise errors.too_long(int(duration), lim.max_duration_seconds)
        est = estimate_bytes(info, PRESETS[preset])
        if est is not None:
            if est > lim.max_task_bytes:
                raise errors.too_large(est, lim.max_task_bytes)
            if free - est < lim.min_free_bytes:
                raise errors.insufficient_space(free, est + lim.min_free_bytes)

        job = self.jobs.submit(
            principal=principal,
            url=info.get("webpage_url") or url,
            preset=preset,
            subtitles=subtitles,
            title=info.get("title"),
            extractor=info.get("extractor_key") or info.get("extractor"),
            estimated_bytes=est,
        )
        return {"created": True, "job": job.view()}

    def get(self, job_id: str) -> dict:
        return self.jobs.get(job_id).view()

    def list(self, state: str | None, limit: int) -> dict:
        return {"jobs": self.jobs.list(state=state, limit=limit)}

    def cancel(self, job_id: str) -> dict:
        return self.jobs.cancel(job_id).view()

    def remove(self, job_id: str, delete_files: bool) -> dict:
        return self.jobs.remove(job_id, delete_files=delete_files)
