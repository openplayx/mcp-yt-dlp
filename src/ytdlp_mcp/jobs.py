"""Download jobs: SQLite records, a bounded worker pool, cancel and limits.

A job is created only after the URL passed the pre-checks in the service
layer. Workers re-extract the URL (signed media URLs expire) and download
into ``<root>/.incomplete/<job>``; yt-dlp moves finished files to ``<root>``.
Jobs do not survive a restart: queued/running ones become ``interrupted``
and their partial files are removed.
"""

from __future__ import annotations

import json
import logging
import shutil
import sqlite3
import threading
import time
import uuid
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

from . import errors
from .config import Config
from .engine import Abort, DownloadSpec, Engine, EngineError

log = logging.getLogger("ytdlp_mcp.jobs")

ACTIVE = ("queued", "running")
FINAL = ("completed", "failed", "canceled", "interrupted")
INCOMPLETE = ".incomplete"

DiskUsage = Callable[[str], tuple[int, int, int]]


@dataclass
class Job:
    id: str
    principal: str
    url: str
    preset: str
    subtitles: list[str]
    state: str = "queued"
    title: str | None = None
    extractor: str | None = None
    estimated_bytes: int | None = None
    created_at: float = 0.0
    started_at: float | None = None
    finished_at: float | None = None
    progress: dict = field(default_factory=dict)
    files: list[dict] = field(default_factory=list)
    error: dict | None = None
    cancel: threading.Event = field(default_factory=threading.Event, repr=False)

    def view(self) -> dict:
        p = dict(self.progress)
        done = p.get("downloaded_bytes")
        if self.state == "completed":
            p["percent"] = 100.0
        elif done and self.estimated_bytes:
            p["percent"] = round(min(99.9, done * 100 / self.estimated_bytes), 1)
        return {
            "job_id": self.id,
            "state": self.state,
            "url": self.url,
            "preset": self.preset,
            "subtitles": self.subtitles,
            "title": self.title,
            "extractor": self.extractor,
            "estimated_bytes": self.estimated_bytes,
            "created_at": _iso(self.created_at),
            "started_at": _iso(self.started_at),
            "finished_at": _iso(self.finished_at),
            "progress": p,
            "files": self.files,
            "error": self.error,
        }


def _iso(ts: float | None) -> str | None:
    if ts is None:
        return None
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))


_SCHEMA = """
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, principal TEXT, url TEXT, preset TEXT, subtitles TEXT,
  state TEXT, title TEXT, extractor TEXT, estimated_bytes INTEGER,
  created_at REAL, started_at REAL, finished_at REAL,
  progress TEXT, files TEXT, error TEXT
);
CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created_at);
"""
_COLS = (
    "id principal url preset subtitles state title extractor estimated_bytes "
    "created_at started_at finished_at progress files error"
).split()
_JSON = {"subtitles", "progress", "files", "error"}


class Store:
    def __init__(self, path: str) -> None:
        if path != ":memory:":
            Path(path).parent.mkdir(parents=True, exist_ok=True)
        self._db = sqlite3.connect(path, check_same_thread=False)
        self._db.executescript(_SCHEMA)
        self._lock = threading.Lock()
        self._closed = False

    def save(self, job: Job) -> None:
        row = []
        for c in _COLS:
            v = getattr(job, c)
            row.append(json.dumps(v, ensure_ascii=False) if c in _JSON else v)
        with self._lock:
            if self._closed:
                return
            with self._db:
                self._db.execute(
                    f"INSERT OR REPLACE INTO jobs ({','.join(_COLS)}) "
                    f"VALUES ({','.join('?' * len(_COLS))})",
                    row,
                )

    def delete(self, job_id: str) -> None:
        with self._lock:
            if self._closed:
                return
            with self._db:
                self._db.execute("DELETE FROM jobs WHERE id = ?", (job_id,))

    def load_all(self) -> list[Job]:
        with self._lock:
            rows = self._db.execute(
                f"SELECT {','.join(_COLS)} FROM jobs ORDER BY created_at"
            ).fetchall()
        jobs = []
        for row in rows:
            kw = {
                c: (json.loads(v) if c in _JSON and v is not None else v)
                for c, v in zip(_COLS, row, strict=True)
            }
            jobs.append(Job(**kw))
        return jobs

    def close(self) -> None:
        with self._lock:
            self._closed = True
            self._db.close()


class JobManager:
    def __init__(
        self,
        config: Config,
        engine: Engine,
        store: Store,
        *,
        clock: Callable[[], float] = time.time,
        disk_usage: DiskUsage = shutil.disk_usage,
    ) -> None:
        self._config = config
        self._limits = config.limits
        self._engine = engine
        self._store = store
        self._clock = clock
        self._disk_usage = disk_usage
        self.root = Path(config.storage.download_root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._jobs: dict[str, Job] = {}
        self._stopping = False
        self._idle = threading.Condition(self._lock)
        self._running = 0
        self._pool = ThreadPoolExecutor(
            max_workers=self._limits.max_concurrent_downloads,
            thread_name_prefix="download",
        )
        self._recover()

    # -- lifecycle -----------------------------------------------------------

    def _recover(self) -> None:
        for job in self._store.load_all():
            if job.state in ACTIVE:
                job.state = "interrupted"
                job.finished_at = job.finished_at or self._clock()
                job.error = {
                    "code": "interrupted",
                    "message": "The service restarted during this download.",
                }
                self._store.save(job)
            self._jobs[job.id] = job
        shutil.rmtree(self.root / INCOMPLETE, ignore_errors=True)

    def shutdown(self, timeout: float = 10.0) -> None:
        """Stops workers at their next progress callback; running jobs end as
        ``interrupted``. Waits up to ``timeout`` (an ffmpeg merge cannot be
        interrupted); the store ignores writes after it is closed anyway."""
        with self._lock:
            self._stopping = True
            for job in self._jobs.values():
                job.cancel.set()
        self._pool.shutdown(wait=False, cancel_futures=True)
        with self._idle:
            self._idle.wait_for(lambda: self._running == 0, timeout)

    # -- queries -------------------------------------------------------------

    def free_bytes(self) -> int:
        return self._disk_usage(str(self.root))[2]

    def get(self, job_id: str) -> Job:
        with self._lock:
            job = self._jobs.get(job_id)
        if job is None:
            raise errors.job_not_found()
        return job

    def list(self, *, state: str | None, limit: int) -> list[dict]:
        with self._lock:
            jobs = sorted(self._jobs.values(), key=lambda j: j.created_at, reverse=True)
            if state == "active":
                jobs = [j for j in jobs if j.state in ACTIVE]
            elif state:
                jobs = [j for j in jobs if j.state == state]
            return [j.view() for j in jobs[:limit]]

    def counts(self) -> dict:
        with self._lock:
            out = {s: 0 for s in (*ACTIVE, *FINAL)}
            for j in self._jobs.values():
                out[j.state] = out.get(j.state, 0) + 1
            return out

    def find_active(self, url: str, preset: str) -> Job | None:
        with self._lock:
            for j in self._jobs.values():
                if j.state in ACTIVE and j.url == url and j.preset == preset:
                    return j
        return None

    # -- commands ------------------------------------------------------------

    def submit(
        self,
        *,
        principal: str,
        url: str,
        preset: str,
        subtitles: list[str],
        title: str | None,
        extractor: str | None,
        estimated_bytes: int | None,
    ) -> Job:
        with self._lock:
            active = sum(1 for j in self._jobs.values() if j.state in ACTIVE)
            if active >= self._limits.max_active_jobs:
                raise errors.queue_full(self._limits.max_active_jobs)
            job = Job(
                id=uuid.uuid4().hex[:16],
                principal=principal,
                url=url,
                preset=preset,
                subtitles=subtitles,
                title=title,
                extractor=extractor,
                estimated_bytes=estimated_bytes,
                created_at=self._clock(),
            )
            self._jobs[job.id] = job
        self._store.save(job)
        self._pool.submit(self._run, job)
        log.info("job queued id=%s extractor=%s preset=%s", job.id, extractor, preset)
        return job

    def cancel(self, job_id: str) -> Job:
        job = self.get(job_id)
        with self._lock:
            if job.state not in ACTIVE:
                return job
            job.cancel.set()
            if job.state == "queued":
                self._finish(job, "canceled", None)
        return job

    def remove(self, job_id: str, *, delete_files: bool) -> dict:
        job = self.get(job_id)
        if job.state in ACTIVE:
            raise errors.job_active()
        deleted: list[str] = []
        freed = 0
        if delete_files:
            for f in job.files:
                path = (self.root / f["path"]).resolve()
                if self.root not in path.parents or not path.is_file():
                    continue
                freed += path.stat().st_size
                path.unlink()
                deleted.append(f["path"])
                parent = path.parent
                if parent != self.root and not any(parent.iterdir()):
                    parent.rmdir()
        with self._lock:
            self._jobs.pop(job_id, None)
        self._store.delete(job_id)
        log.info("job removed id=%s files=%d", job_id, len(deleted))
        return {
            "job_id": job_id,
            "removed": True,
            "deleted_files": deleted,
            "freed_bytes": freed,
            "kept_files": [] if delete_files else [f["path"] for f in job.files],
        }

    # -- worker --------------------------------------------------------------

    def _finish(self, job: Job, state: str, error: dict | None) -> None:
        job.state = state
        job.error = error
        job.finished_at = self._clock()
        self._store.save(job)

    def _run(self, job: Job) -> None:
        with self._lock:
            if job.state != "queued" or job.cancel.is_set():
                return
            job.state = "running"
            job.started_at = self._clock()
            self._running += 1
        try:
            self._store.save(job)
            self._download(job)
        finally:
            with self._idle:
                self._running -= 1
                self._idle.notify_all()

    def _download(self, job: Job) -> None:
        temp = self.root / INCOMPLETE / job.id
        spec = DownloadSpec(
            url=job.url,
            preset=job.preset,
            subtitles=job.subtitles,
            home=self.root,
            temp=temp,
        )
        state, error, files = "failed", None, []
        try:
            paths = self._engine.download(spec, self._progress_fn(job))
            files = [
                {"path": str(p.relative_to(self.root)), "bytes": p.stat().st_size}
                for p in paths
            ]
            if files:
                state = "completed"
            else:
                error = {
                    "code": "no_output",
                    "message": "yt-dlp finished without producing a file "
                    "(it may already exist or exceed max_filesize).",
                }
        except Abort as a:
            if a.code == "canceled" and self._stopping:
                state = "interrupted"
                error = {
                    "code": "interrupted",
                    "message": "The service stopped during this download.",
                }
            else:
                state = "canceled" if a.code == "canceled" else "failed"
                error = {"code": a.code, "message": a.message}
        except EngineError as e:
            error = {"code": e.code, "message": e.message}
        except Exception:
            log.exception("job crashed id=%s", job.id)
            error = {"code": "internal_error", "message": "Unexpected server error."}
        finally:
            shutil.rmtree(temp, ignore_errors=True)
        with self._lock:
            job.files = files
            self._finish(job, state, error)
        log.info(
            "job finished id=%s state=%s error=%s files=%d",
            job.id,
            state,
            (error or {}).get("code", "-"),
            len(files),
        )

    def _progress_fn(self, job: Job) -> Callable[[dict], None]:
        lim = self._limits
        per_file: dict[str, int] = {}
        last = {"save": 0.0, "disk": 0.0}

        def update(d: dict) -> None:
            if job.cancel.is_set():
                raise Abort("canceled", "Canceled by request.")
            name = d.get("filename")
            if name and d.get("downloaded_bytes") is not None:
                per_file[name] = int(d["downloaded_bytes"])
            total = sum(per_file.values())
            if total > lim.max_task_bytes:
                raise Abort(
                    "too_large",
                    f"Stopped after {total} bytes: above the per-download limit "
                    f"of {lim.max_task_bytes} bytes.",
                )
            now = self._clock()
            if now - last["disk"] >= 2:
                last["disk"] = now
                free = self.free_bytes()
                if free < lim.min_free_bytes:
                    raise Abort(
                        "insufficient_space",
                        f"Stopped: free space fell to {free} bytes, below the "
                        f"{lim.min_free_bytes}-byte reserve.",
                    )
            with self._lock:
                job.progress = {
                    "phase": d.get("phase"),
                    "downloaded_bytes": total,
                    "current_total_bytes": d.get("total_bytes"),
                    "speed_bps": int(d["speed"]) if d.get("speed") else None,
                    "eta_seconds": d.get("eta"),
                }
                if d.get("postprocessor"):
                    job.progress["postprocessor"] = d["postprocessor"]
            if now - last["save"] >= 5:
                last["save"] = now
                self._store.save(job)

        return update
