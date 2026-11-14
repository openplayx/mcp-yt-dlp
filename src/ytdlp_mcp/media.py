"""Quality presets and the compact media summary returned to clients.

Clients never pass raw yt-dlp format strings or options; they pick one of the
presets below. Size estimates are approximate (the site's declared sizes or
bitrate x duration) and only used for limit checks and display.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class Preset:
    key: str
    label: str
    format: str
    max_height: int | None = None
    audio_only: bool = False


def _capped(h: int) -> str:
    # Best at or below the cap; if the site has nothing that small, the
    # smallest available instead of failing.
    return f"bv*[height<={h}]+ba/b[height<={h}]/wv*+ba/w"


PRESETS: dict[str, Preset] = {
    p.key: p
    for p in (
        Preset("best", "Best video + audio", "bv*+ba/b"),
        Preset("2160p", "Up to 2160p", _capped(2160), 2160),
        Preset("1080p", "Up to 1080p", _capped(1080), 1080),
        Preset("720p", "Up to 720p", _capped(720), 720),
        Preset("480p", "Up to 480p", _capped(480), 480),
        Preset("audio", "Audio only (m4a)", "ba/b", audio_only=True),
    )
}
DEFAULT_PRESET = "1080p"


def _size(f: dict, duration: float | None) -> int | None:
    size = f.get("filesize") or f.get("filesize_approx")
    if size:
        return int(size)
    tbr = f.get("tbr")
    if tbr and duration:
        return int(tbr * 1000 / 8 * duration)
    return None


def _has_video(f: dict) -> bool:
    return f.get("vcodec") not in (None, "none") or (
        f.get("vcodec") is None and bool(f.get("height"))
    )


def _has_audio(f: dict) -> bool:
    return f.get("acodec") not in (None, "none")


def estimate_bytes(info: dict, preset: Preset) -> int | None:
    formats = [f for f in info.get("formats") or [] if isinstance(f, dict)]
    duration = info.get("duration")
    if not formats:
        return _size(info, duration)
    audio = [f for f in formats if _has_audio(f) and not _has_video(f)]
    best_audio = max(
        audio, key=lambda f: f.get("abr") or f.get("tbr") or 0, default=None
    )
    if preset.audio_only:
        pick = best_audio or max(formats, key=lambda f: f.get("tbr") or 0)
        return _size(pick, duration)
    video = [f for f in formats if _has_video(f)]
    if preset.max_height:
        capped = [f for f in video if (f.get("height") or 0) <= preset.max_height]
        video = capped or sorted(video, key=lambda f: f.get("height") or 0)[:1]
    if not video:
        return None
    pick = max(video, key=lambda f: (f.get("height") or 0, f.get("tbr") or 0))
    size = _size(pick, duration)
    if size is None:
        return None
    if not _has_audio(pick) and best_audio is not None:
        extra = _size(best_audio, duration)
        size += extra or 0
    return size


def _text(value: Any, limit: int) -> str | None:
    if not isinstance(value, str) or not value:
        return None
    return value if len(value) <= limit else value[: limit - 1] + "…"


def summarize(info: dict, *, max_task_bytes: int, max_entries: int = 25) -> dict:
    kind = info.get("_type") or "video"
    base = {
        "id": info.get("id"),
        "title": _text(info.get("title"), 300),
        "extractor": info.get("extractor_key") or info.get("extractor"),
        "webpage_url": info.get("webpage_url") or info.get("original_url"),
        "uploader": _text(info.get("uploader") or info.get("channel"), 200),
    }
    if kind in ("playlist", "multi_video"):
        entries = [e for e in info.get("entries") or [] if isinstance(e, dict)]
        count = info.get("playlist_count") or len(entries)
        return {
            "kind": "playlist",
            **base,
            "entry_count": count,
            "entries": [
                {
                    "id": e.get("id"),
                    "title": _text(e.get("title"), 300),
                    "url": e.get("webpage_url") or e.get("url"),
                    "duration": e.get("duration"),
                }
                for e in entries[:max_entries]
            ],
            "entries_truncated": len(entries) > max_entries,
            "note": "Playlists are not downloaded as a whole; pass one entry URL "
            "to start_download.",
        }
    formats = [f for f in info.get("formats") or [] if isinstance(f, dict)]
    heights = sorted(
        {int(f["height"]) for f in formats if _has_video(f) and f.get("height")},
        reverse=True,
    )
    presets = []
    for p in PRESETS.values():
        est = estimate_bytes(info, p)
        presets.append(
            {
                "preset": p.key,
                "label": p.label,
                "estimated_bytes": est,
                "within_limit": None if est is None else est <= max_task_bytes,
            }
        )
    subs = sorted((info.get("subtitles") or {}).keys())
    return {
        "kind": "video",
        **base,
        "duration": info.get("duration"),
        "upload_date": info.get("upload_date"),
        "live_status": info.get("live_status"),
        "thumbnail": info.get("thumbnail"),
        "view_count": info.get("view_count"),
        "description": _text(info.get("description"), 800),
        "available_heights": heights[:12],
        "has_audio": any(_has_audio(f) for f in formats) if formats else None,
        "subtitle_languages": subs[:40],
        "auto_caption_languages_count": len(info.get("automatic_captions") or {}),
        "presets": presets,
    }
