from __future__ import annotations

import threading
import time
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from ytdlp_mcp.app import create_app
from ytdlp_mcp.config import Config, Secrets
from ytdlp_mcp.engine import DownloadSpec, EngineError

TOKEN = "t" * 32
READ_TOKEN = "r" * 32
GIB = 1024**3


def video(**kw) -> dict:
    info = {
        "id": "abc123",
        "title": "A video",
        "extractor_key": "Youtube",
        "webpage_url": "https://www.youtube.com/watch?v=abc123",
        "duration": 60,
        "live_status": "not_live",
        "formats": [
            {
                "format_id": "v1",
                "vcodec": "avc1",
                "acodec": "none",
                "height": 1080,
                "filesize": 50_000_000,
                "tbr": 4000,
            },
            {
                "format_id": "v2",
                "vcodec": "avc1",
                "acodec": "none",
                "height": 720,
                "filesize": 20_000_000,
                "tbr": 2000,
            },
            {
                "format_id": "a1",
                "vcodec": "none",
                "acodec": "mp4a",
                "abr": 128,
                "filesize": 1_000_000,
            },
        ],
        "subtitles": {"en": [{}]},
    }
    info.update(kw)
    return info


class FakeEngine:
    version = "test"

    def __init__(self) -> None:
        self.info: dict | Exception = video()
        self.chunks = [1000, 2000]
        self.gate: threading.Event | None = None
        self.started = threading.Event()
        self.specs: list[DownloadSpec] = []

    def probe(self, url: str) -> dict:
        if isinstance(self.info, Exception):
            raise self.info
        return self.info

    def download(self, spec: DownloadSpec, progress) -> list[Path]:
        self.specs.append(spec)
        spec.temp.mkdir(parents=True, exist_ok=True)
        (spec.temp / "part").write_bytes(b"x")
        self.started.set()
        done = 0
        for c in self.chunks:
            done += c
            progress(
                {
                    "phase": "downloading",
                    "filename": "f.part",
                    "downloaded_bytes": done,
                    "total_bytes": sum(self.chunks),
                }
            )
            if self.gate is not None:
                while not self.gate.wait(0.01):
                    progress(
                        {
                            "phase": "downloading",
                            "filename": "f.part",
                            "downloaded_bytes": done,
                        }
                    )
        out = spec.home / "Youtube" / "A video [abc123].mp4"
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_bytes(b"v" * done)
        sub = out.with_name("A video [abc123].en.srt")
        sub.write_text("1\n")
        return [out, sub]


def public_resolver(host: str, port: int) -> list[str]:
    return {"lan.example": ["192.168.1.5"]}.get(host, ["93.184.216.34"])


@pytest.fixture
def env(tmp_path):
    state = {"free": 100 * GIB}
    engine = FakeEngine()

    def make(**limits):
        config = Config.model_validate(
            {
                "storage": {
                    "database": str(tmp_path / "db.sqlite3"),
                    "download_root": str(tmp_path / "dl"),
                    "cache_dir": str(tmp_path / "cache"),
                },
                "limits": {"min_free_bytes": 1 * GIB, **limits},
            }
        )
        secrets = Secrets.model_validate(
            {
                "tokens": [
                    {"name": "app", "token": TOKEN, "scopes": ["read", "download"]},
                    {"name": "ro", "token": READ_TOKEN, "scopes": ["read"]},
                ]
            }
        )
        app = create_app(
            config,
            secrets,
            engine=engine,
            resolver=public_resolver,
            disk_usage=lambda p: (0, 0, state["free"]),
        )
        return TestClient(app)

    return make, engine, state, tmp_path


def rpc(client, method, params=None, token=TOKEN, rid=1):
    body = {"jsonrpc": "2.0", "id": rid, "method": method}
    if params is not None:
        body["params"] = params
    r = client.post("/mcp", json=body, headers={"Authorization": f"Bearer {token}"})
    return r


def call(client, name, args=None, token=TOKEN):
    r = rpc(client, "tools/call", {"name": name, "arguments": args or {}}, token)
    assert r.status_code == 200, r.text
    res = r.json()["result"]
    return res["structuredContent"], res["isError"]


def wait_state(client, job_id, states=("completed", "failed", "canceled")):
    for _ in range(300):
        job, _ = call(client, "get_download", {"job_id": job_id})
        if job["state"] in states:
            return job
        time.sleep(0.01)
    raise AssertionError(job)


def test_handshake_and_tools(env):
    make, *_ = env
    with make() as c:
        r = rpc(c, "initialize", {"protocolVersion": "2025-06-18"})
        res = r.json()["result"]
        assert res["protocolVersion"] == "2025-06-18"
        assert res["serverInfo"]["name"] == "openplayx-yt-dlp"
        tools = {t["name"]: t for t in rpc(c, "tools/list").json()["result"]["tools"]}
        assert set(tools) == {
            "get_status",
            "get_media_info",
            "start_download",
            "list_downloads",
            "get_download",
            "cancel_download",
            "remove_download",
        }
        assert tools["get_media_info"]["annotations"]["readOnlyHint"] is True
        assert tools["start_download"]["annotations"]["readOnlyHint"] is False
        assert tools["remove_download"]["annotations"]["destructiveHint"] is True
        ro = {
            t["name"]
            for t in rpc(c, "tools/list", token=READ_TOKEN).json()["result"]["tools"]
        }
        assert ro == {"get_status", "get_media_info", "list_downloads", "get_download"}
        r = rpc(
            c,
            "tools/call",
            {"name": "start_download", "arguments": {"url": "https://x.com/v"}},
            token=READ_TOKEN,
        )
        assert r.json()["error"]["code"] == -32602


def test_auth_and_transport(env):
    make, *_ = env
    with make() as c:
        assert c.post("/mcp", json={}).status_code == 401
        assert rpc(c, "ping", token="w" * 32).status_code == 401
        assert c.get("/mcp").status_code == 405
        r = c.post(
            "/mcp",
            json={"jsonrpc": "2.0", "method": "notifications/x"},
            headers={"Authorization": f"Bearer {TOKEN}"},
        )
        assert r.status_code == 202
        assert c.get("/healthz").json()["status"] == "ok"


def test_media_info(env):
    make, engine, *_ = env
    with make() as c:
        out, err = call(
            c, "get_media_info", {"url": "https://www.youtube.com/watch?v=abc123"}
        )
        assert not err
        assert out["kind"] == "video" and out["available_heights"] == [1080, 720]
        presets = {p["preset"]: p["estimated_bytes"] for p in out["presets"]}
        assert presets["1080p"] == 51_000_000
        assert presets["720p"] == 21_000_000
        assert presets["audio"] == 1_000_000
        engine.info = {
            "_type": "playlist",
            "title": "L",
            "playlist_count": 2,
            "entries": [{"id": "1", "url": "https://y/1", "title": "one"}],
        }
        out, err = call(
            c, "get_media_info", {"url": "https://www.youtube.com/playlist?list=x"}
        )
        assert out["kind"] == "playlist" and out["entry_count"] == 2
        engine.info = EngineError("unsupported_url", "Unsupported URL: x")
        out, err = call(c, "get_media_info", {"url": "https://example.org/"})
        assert err and out["error"]["code"] == "unsupported_url"


@pytest.mark.parametrize(
    "url",
    [
        "file:///etc/passwd",
        "http://127.0.0.1/x",
        "http://lan.example/v",
        "http://localhost:8800/mcp",
        "https://user:pw@example.com/v",
        "ftp://x/y",
    ],
)
def test_blocked_urls(env, url):
    make, *_ = env
    with make() as c:
        out, err = call(c, "start_download", {"url": url})
        assert err and out["error"]["code"] == "url_blocked"


def test_download_lifecycle(env):
    make, engine, state, tmp = env
    with make() as c:
        out, err = call(
            c,
            "start_download",
            {
                "url": "https://www.youtube.com/watch?v=abc123",
                "preset": "720p",
                "subtitles": ["en"],
            },
        )
        assert not err and out["created"]
        job = wait_state(c, out["job"]["job_id"])
        assert job["state"] == "completed", job
        assert [f["path"] for f in job["files"]] == [
            "Youtube/A video [abc123].mp4",
            "Youtube/A video [abc123].en.srt",
        ]
        assert job["estimated_bytes"] == 21_000_000
        assert job["progress"]["phase"] == "completed"
        assert job["progress"]["percent"] == 100.0
        assert engine.specs[0].subtitles == ["en"]
        assert not (tmp / "dl" / ".incomplete" / job["job_id"]).exists()
        listed, _ = call(c, "list_downloads", {"state": "completed"})
        assert [j["job_id"] for j in listed["jobs"]] == [job["job_id"]]
        status, _ = call(c, "get_status")
        assert status["jobs"]["completed"] == 1

        out, err = call(
            c, "remove_download", {"job_id": job["job_id"], "delete_files": True}
        )
        assert not err and len(out["deleted_files"]) == 2
        assert not (tmp / "dl" / "Youtube").exists()
        out, err = call(c, "get_download", {"job_id": job["job_id"]})
        assert err and out["error"]["code"] == "not_found"


def test_cancel_running_and_dedupe(env):
    make, engine, *_ = env
    engine.gate = threading.Event()
    with make() as c:
        args = {"url": "https://www.youtube.com/watch?v=abc123"}
        out, _ = call(c, "start_download", args)
        jid = out["job"]["job_id"]
        assert engine.started.wait(2)
        again, _ = call(c, "start_download", args)
        assert again["created"] is False and again["job"]["job_id"] == jid
        out, err = call(c, "remove_download", {"job_id": jid})
        assert err and out["error"]["code"] == "job_active"
        call(c, "cancel_download", {"job_id": jid})
        job = wait_state(c, jid)
        assert job["state"] == "canceled" and job["files"] == []
        assert job["error"]["code"] == "canceled"


def test_limits(env):
    make, engine, state, _ = env
    url = {"url": "https://www.youtube.com/watch?v=abc123"}
    with make(max_task_bytes=30_000_000, max_duration_seconds=3600) as c:
        out, err = call(c, "start_download", {**url, "preset": "best"})
        assert err and out["error"]["code"] == "too_large"
        engine.info = video(duration=7200)
        out, err = call(c, "start_download", url)
        assert err and out["error"]["code"] == "too_long"
        engine.info = video(live_status="is_live")
        out, err = call(c, "start_download", url)
        assert err and out["error"]["code"] == "live_not_supported"
        engine.info = {"_type": "playlist", "entries": [{}, {}]}
        out, err = call(c, "start_download", url)
        assert err and out["error"]["code"] == "playlist_not_supported"
        engine.info = video()
        state["free"] = int(1.01 * GIB)
        out, err = call(c, "start_download", {**url, "preset": "720p"})
        assert err and out["error"]["code"] == "insufficient_space"
        out, err = call(c, "start_download", {**url, "preset": "8k"})
        assert err and out["error"]["code"] == "invalid_request"


def test_abort_when_download_exceeds_limit(env):
    make, engine, *_ = env
    engine.info = video(formats=[])  # unknown size: only the runtime check applies
    engine.chunks = [20_000_000, 20_000_000]
    with make(max_task_bytes=30_000_000) as c:
        out, _ = call(
            c, "start_download", {"url": "https://www.youtube.com/watch?v=abc123"}
        )
        job = wait_state(c, out["job"]["job_id"])
        assert job["state"] == "failed" and job["error"]["code"] == "too_large"


def test_restart_marks_interrupted(env):
    make, engine, *_ = env
    engine.gate = threading.Event()
    with make() as c:
        out, _ = call(
            c, "start_download", {"url": "https://www.youtube.com/watch?v=abc123"}
        )
        jid = out["job"]["job_id"]
        assert engine.started.wait(2)
    engine.gate.set()
    engine.gate = None
    with make() as c:
        job, _ = call(c, "get_download", {"job_id": jid})
        assert job["state"] == "interrupted", job
