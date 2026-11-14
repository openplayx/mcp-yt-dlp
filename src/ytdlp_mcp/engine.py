"""The only module that talks to yt-dlp.

``Engine`` is the seam tests replace. ``YtDlpEngine`` builds a fresh
``YoutubeDL`` per call with options derived from config and the chosen
preset only; no caller-supplied yt-dlp options ever reach it.
"""

from __future__ import annotations

import logging
import re
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from .config import Config
from .media import PRESETS

log = logging.getLogger("ytdlp_mcp.engine")

_ANSI = re.compile(r"\x1b\[[0-9;]*m")
OUTTMPL = "%(extractor_key)s/%(title).150B [%(id)s].%(ext)s"


class EngineError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


class Abort(Exception):  # noqa: N818 - control flow, not an error report
    """Raised by progress callbacks to stop a download (cancel or a limit)."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


@dataclass
class DownloadSpec:
    url: str
    preset: str
    subtitles: list[str] = field(default_factory=list)
    home: Path = Path(".")
    temp: Path = Path(".")


ProgressFn = Callable[[dict], None]


class Engine(Protocol):
    version: str

    def probe(self, url: str) -> dict: ...

    def download(self, spec: DownloadSpec, progress: ProgressFn) -> list[Path]: ...


def clean_message(text: str, limit: int = 400) -> str:
    text = _ANSI.sub("", str(text)).strip()
    text = re.sub(r"^(ERROR:\s*)+", "", text)
    return text[:limit] or "Unknown error."


class _Logger:
    """Routes yt-dlp output to logging; never to stdout."""

    def debug(self, msg: str) -> None:
        if not msg.startswith("[debug] "):
            log.debug(clean_message(msg, 1000))

    info = debug

    def warning(self, msg: str) -> None:
        log.info("yt-dlp warning: %s", clean_message(msg, 500))

    def error(self, msg: str) -> None:
        log.info("yt-dlp error: %s", clean_message(msg, 500))


def _map_error(exc: Exception) -> EngineError:
    from yt_dlp.utils import DownloadError, UnsupportedError

    inner = exc
    if isinstance(exc, DownloadError) and exc.exc_info and exc.exc_info[1]:
        inner = exc.exc_info[1]
    msg = clean_message(str(inner) or str(exc))
    if isinstance(inner, UnsupportedError) or any(
        m in msg for m in ("Unsupported URL", "No suitable extractor")
    ):
        return EngineError("unsupported_url", msg)
    if "timed out" in msg.lower():
        return EngineError("upstream_timeout", msg)
    return EngineError("extraction_failed", msg)


class YtDlpEngine:
    def __init__(self, config: Config, proxy: str | None = None) -> None:
        from yt_dlp.version import __version__

        self.version = __version__
        self._config = config
        self._proxy = proxy

    def _base_opts(self) -> dict:
        net = self._config.network
        extractors = [e.lower() for e in net.allowed_extractors] or ["default"]
        if not net.allow_generic_extractor:
            extractors.append("-generic")
        opts = {
            "quiet": True,
            "no_warnings": False,
            "noprogress": True,
            "logger": _Logger(),
            "socket_timeout": net.socket_timeout_seconds,
            "cachedir": self._config.storage.cache_dir,
            "allowed_extractors": extractors,
            "noplaylist": True,
            "windowsfilenames": True,
            "overwrites": False,
            "color": {"stdout": "no_color", "stderr": "no_color"},
        }
        if self._proxy:
            opts["proxy"] = self._proxy
        if net.rate_limit_bytes_per_second:
            opts["ratelimit"] = net.rate_limit_bytes_per_second
        return opts

    def probe(self, url: str) -> dict:
        from yt_dlp import YoutubeDL

        opts = self._base_opts() | {
            "extract_flat": "in_playlist",
            "format": PRESETS["best"].format,
            "ignore_no_formats_error": True,
            "skip_download": True,
        }
        try:
            with YoutubeDL(opts) as ydl:
                info = ydl.extract_info(url, download=False)
                return ydl.sanitize_info(info)
        except Exception as exc:
            raise _map_error(exc) from exc

    def download(self, spec: DownloadSpec, progress: ProgressFn) -> list[Path]:
        from yt_dlp import YoutubeDL
        from yt_dlp.utils import DownloadCancelled

        class Stop(DownloadCancelled):
            def __init__(self, abort: Abort) -> None:
                super().__init__(abort.message)
                self.abort = abort

        def hook(d: dict) -> None:
            try:
                progress(
                    {
                        "phase": "downloading"
                        if d.get("status") != "finished"
                        else "downloaded",
                        "filename": d.get("filename"),
                        "downloaded_bytes": d.get("downloaded_bytes"),
                        "total_bytes": d.get("total_bytes")
                        or d.get("total_bytes_estimate"),
                        "speed": d.get("speed"),
                        "eta": d.get("eta"),
                    }
                )
            except Abort as a:
                raise Stop(a) from a

        def pp_hook(d: dict) -> None:
            if d.get("status") == "started":
                try:
                    progress(
                        {
                            "phase": "postprocessing",
                            "postprocessor": d.get("postprocessor"),
                        }
                    )
                except Abort as a:
                    raise Stop(a) from a

        finals: list[str] = []
        preset = PRESETS[spec.preset]
        lim = self._config.limits
        opts = self._base_opts() | {
            "format": preset.format,
            "merge_output_format": "mp4/mkv",
            "outtmpl": {"default": OUTTMPL},
            "paths": {"home": str(spec.home), "temp": str(spec.temp)},
            # Belt and braces: playlists are rejected before queueing.
            "playlist_items": "1",
            "max_filesize": lim.max_task_bytes,
            "progress_hooks": [hook],
            "postprocessor_hooks": [pp_hook],
            "post_hooks": [finals.append],
            "postprocessors": [],
        }
        if preset.audio_only:
            opts["postprocessors"].append(
                {"key": "FFmpegExtractAudio", "preferredcodec": "m4a"}
            )
        if spec.subtitles:
            opts |= {"writesubtitles": True, "subtitleslangs": spec.subtitles}
            opts["postprocessors"].append(
                {
                    "key": "FFmpegSubtitlesConvertor",
                    "format": "srt",
                    "when": "before_dl",
                }
            )
        try:
            with YoutubeDL(opts) as ydl:
                ydl.extract_info(spec.url, download=True)
        except Exception as exc:
            stop = exc
            if hasattr(exc, "exc_info") and exc.exc_info and exc.exc_info[1]:
                stop = exc.exc_info[1]
            if isinstance(stop, Stop):
                raise stop.abort from exc
            if isinstance(exc, Stop):
                raise exc.abort from exc
            raise _map_error(exc) from exc
        return _collect(finals, spec.home)


def _collect(finals: list[str], home: Path) -> list[Path]:
    """Main files from post_hooks plus their sidecars (same stem: subtitles)."""
    home = home.resolve()
    out: list[Path] = []
    for raw in finals:
        main = Path(raw).resolve()
        if not main.is_file() or home not in main.parents:
            continue
        stem = main.stem
        for p in sorted(main.parent.iterdir()):
            if p.is_file() and (p == main or p.name.startswith(stem + ".")):
                if p not in out:
                    out.append(p)
    return out
