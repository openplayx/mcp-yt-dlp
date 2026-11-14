"""MCP entry point (Streamable HTTP, ``POST /mcp``).

Same deliberately small subset as the other OpenPlayX MCP services:

* JSON-RPC 2.0 single messages over ``POST``; ``application/json`` responses
  (no SSE stream, no server-initiated messages).
* Stateless: no ``Mcp-Session-Id``; every request carries the bearer token.
* ``initialize``, ``ping``, ``tools/list``, ``tools/call`` and notifications.
* ``GET``/``DELETE`` answer 405.

Business failures are tool results with ``isError: true`` and a stable error
code, so the model can see and explain them. Tools that change server state
are not marked read-only; OpenPlayX asks the user to confirm those.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Literal

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, Response
from pydantic import BaseModel, ConfigDict, Field, ValidationError

from . import __version__, errors
from .auth import Principal
from .media import DEFAULT_PRESET, PRESETS
from .service import Service

log = logging.getLogger("ytdlp_mcp")

PROTOCOL_VERSIONS = ("2025-11-25", "2025-06-18", "2025-03-26")
SERVER_NAME = "openplayx-yt-dlp"
INSTRUCTIONS = (
    "Downloads online videos (YouTube and the other sites yt-dlp supports) to "
    "the user's own server. get_media_info inspects a URL without downloading. "
    "start_download queues one video at a chosen quality preset and returns a "
    "job_id; downloads run in the background, so poll get_download for "
    "progress. Playlists and live streams are not downloaded as a whole. The "
    "user must have the right to download the content."
)

PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602

router = APIRouter()

PresetKey = Literal[tuple(PRESETS)]  # type: ignore[valid-type]
JOB_ID = Field(min_length=1, max_length=32, pattern=r"^[0-9a-f]+$")


class _Strict(BaseModel):
    model_config = ConfigDict(extra="forbid")


class _NoArgs(_Strict):
    pass


class _UrlArgs(_Strict):
    url: str = Field(min_length=1, max_length=2048)


class _StartArgs(_Strict):
    url: str = Field(min_length=1, max_length=2048)
    preset: PresetKey = DEFAULT_PRESET
    subtitles: list[str] = Field(default_factory=list, max_length=20)

    def langs(self) -> list[str]:
        out = []
        for s in self.subtitles:
            s = s.strip()
            if not s or len(s) > 20 or not all(c.isalnum() or c in "-_" for c in s):
                raise ValueError(f"invalid subtitle language {s!r}")
            if s not in out:
                out.append(s)
        return out


class _ListArgs(_Strict):
    state: (
        Literal[
            "active",
            "queued",
            "running",
            "completed",
            "failed",
            "canceled",
            "interrupted",
        ]
        | None
    ) = None
    limit: int = Field(20, ge=1, le=100)


class _JobArgs(_Strict):
    job_id: str = JOB_ID


class _RemoveArgs(_Strict):
    job_id: str = JOB_ID
    delete_files: bool = False


def _obj(props: dict, required: list[str] | None = None) -> dict:
    out = {"type": "object", "properties": props, "additionalProperties": False}
    if required:
        out["required"] = required
    return out


_URL = {"type": "string", "maxLength": 2048, "description": "Page URL of one video."}
_JOB = {"type": "string", "maxLength": 32, "description": "job_id from start_download."}


@dataclass(frozen=True)
class Tool:
    name: str
    title: str
    description: str
    scope: str
    schema: dict
    args: type[BaseModel]
    run: Callable[[Service, Principal, Any], Awaitable[dict] | dict]
    read_only: bool = True
    destructive: bool = False
    idempotent: bool = True

    def describe(self) -> dict:
        return {
            "name": self.name,
            "title": self.title,
            "description": self.description,
            "inputSchema": self.schema,
            "annotations": {
                "title": self.title,
                "readOnlyHint": self.read_only,
                "destructiveHint": self.destructive,
                "idempotentHint": self.idempotent,
                "openWorldHint": True,
            },
        }


async def _start(svc: Service, who: Principal, a: _StartArgs) -> dict:
    try:
        langs = a.langs()
    except ValueError as exc:
        raise errors.invalid_request(str(exc)) from exc
    return await svc.start(who.name, a.url, a.preset, langs)


TOOLS: tuple[Tool, ...] = (
    Tool(
        name="get_status",
        title="Download service status",
        description=(
            "Free space on the server, size/duration limits, quality presets and "
            "job counts. Read-only."
        ),
        scope="read",
        schema=_obj({}),
        args=_NoArgs,
        run=lambda svc, who, a: svc.status(),
    ),
    Tool(
        name="get_media_info",
        title="Inspect media URL",
        description=(
            "Reads title, uploader, duration, available resolutions, subtitle "
            "languages and estimated size per preset for a video URL; for a "
            "playlist, lists its entries (each entry URL can be downloaded). "
            "Never downloads."
        ),
        scope="read",
        schema=_obj({"url": _URL}, ["url"]),
        args=_UrlArgs,
        run=lambda svc, who, a: svc.info(a.url),
    ),
    Tool(
        name="start_download",
        title="Download video to server",
        description=(
            "Queues a download of one video to the server and returns a job "
            "(job_id, state). Checks size, duration and free space first. If the "
            "same URL and preset is already queued or running, returns that job "
            "instead. Poll get_download for progress and the resulting files."
        ),
        scope="download",
        schema=_obj(
            {
                "url": _URL,
                "preset": {
                    "type": "string",
                    "enum": list(PRESETS),
                    "default": DEFAULT_PRESET,
                    "description": "Quality: best, 2160p/1080p/720p/480p caps, "
                    "or audio (m4a).",
                },
                "subtitles": {
                    "type": "array",
                    "items": {"type": "string", "maxLength": 20},
                    "maxItems": 20,
                    "default": [],
                    "description": "Subtitle language codes to save as .srt next "
                    'to the video, e.g. ["en", "zh-Hans"]. Uploaded subtitles '
                    "only.",
                },
            },
            ["url"],
        ),
        args=_StartArgs,
        run=_start,
        read_only=False,
        idempotent=False,
    ),
    Tool(
        name="list_downloads",
        title="List downloads",
        description="Recent download jobs, newest first. Read-only.",
        scope="read",
        schema=_obj(
            {
                "state": {
                    "type": "string",
                    "enum": [
                        "active",
                        "queued",
                        "running",
                        "completed",
                        "failed",
                        "canceled",
                        "interrupted",
                    ],
                    "description": "Filter; 'active' = queued or running.",
                },
                "limit": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": 100,
                    "default": 20,
                },
            }
        ),
        args=_ListArgs,
        run=lambda svc, who, a: svc.list(a.state, a.limit),
    ),
    Tool(
        name="get_download",
        title="Download progress",
        description=(
            "State, progress, resulting files (paths relative to the download "
            "folder, sizes) and error of one job. Read-only."
        ),
        scope="read",
        schema=_obj({"job_id": _JOB}, ["job_id"]),
        args=_JobArgs,
        run=lambda svc, who, a: svc.get(a.job_id),
    ),
    Tool(
        name="cancel_download",
        title="Cancel download",
        description=(
            "Stops a queued or running job and discards its partial files. "
            "Finished jobs are left unchanged."
        ),
        scope="download",
        schema=_obj({"job_id": _JOB}, ["job_id"]),
        args=_JobArgs,
        run=lambda svc, who, a: svc.cancel(a.job_id),
        read_only=False,
    ),
    Tool(
        name="remove_download",
        title="Remove download",
        description=(
            "Removes a finished job from the list. With delete_files=true also "
            "deletes its downloaded files from the server (cannot be undone)."
        ),
        scope="download",
        schema=_obj(
            {
                "job_id": _JOB,
                "delete_files": {"type": "boolean", "default": False},
            },
            ["job_id"],
        ),
        args=_RemoveArgs,
        run=lambda svc, who, a: svc.remove(a.job_id, a.delete_files),
        read_only=False,
        destructive=True,
    ),
)
_BY_NAME = {t.name: t for t in TOOLS}


def _visible(who: Principal) -> list[Tool]:
    return [t for t in TOOLS if t.scope in who.scopes]


def _rpc_error(rid: Any, code: int, message: str, status: int = 200) -> JSONResponse:
    return JSONResponse(
        {"jsonrpc": "2.0", "id": rid, "error": {"code": code, "message": message}},
        status_code=status,
    )


def _result(rid: Any, result: dict) -> JSONResponse:
    return JSONResponse({"jsonrpc": "2.0", "id": rid, "result": result})


def _tool_result(payload: dict, *, error: bool = False) -> dict:
    return {
        "content": [{"type": "text", "text": json.dumps(payload, ensure_ascii=False)}],
        "structuredContent": payload,
        "isError": error,
    }


def _origin_allowed(request: Request) -> bool:
    # DNS-rebinding guard: a browser page may only call us from our own origin.
    origin = request.headers.get("origin")
    if not origin:
        return True
    host = request.headers.get("host", "")
    return origin.split("://", 1)[-1].rstrip("/") == host


class _ParamsError(Exception):
    pass


async def _call(request: Request, who: Principal, params: Any) -> dict:
    if not isinstance(params, dict) or not isinstance(params.get("name"), str):
        raise _ParamsError("params.name is required.")
    tool = _BY_NAME.get(params["name"])
    if tool is None or tool.scope not in who.scopes:
        raise _ParamsError(f"Unknown tool: {params['name']}")
    raw = params.get("arguments") or {}
    if not isinstance(raw, dict):
        raise _ParamsError("params.arguments must be an object.")
    try:
        args = tool.args.model_validate(raw)
    except ValidationError as exc:
        # Locations and messages only; inputs are not echoed back.
        details = "; ".join(
            f"{'.'.join(str(p) for p in e.get('loc', ())) or 'arguments'}: "
            f"{e.get('msg', '')}"
            for e in exc.errors()[:5]
        )
        return _tool_result(
            errors.invalid_request(details[:500] or "Invalid input.").body(), error=True
        )
    request.state.tool = tool.name
    try:
        out = tool.run(request.app.state.service, who, args)
        if hasattr(out, "__await__"):
            out = await out
        return _tool_result(out)
    except errors.ApiError as exc:
        request.state.error_code = exc.code
        return _tool_result(exc.body(), error=True)


@router.post("/mcp", include_in_schema=False)
async def mcp_post(request: Request) -> Response:
    if not _origin_allowed(request):
        raise errors.ApiError("forbidden", 403, "Origin not allowed.")
    accept = request.headers.get("accept", "*/*")
    if not any(a in accept for a in ("application/json", "*/*")):
        return Response(status_code=406)
    version = request.headers.get("mcp-protocol-version")
    if version and version not in PROTOCOL_VERSIONS:
        return _rpc_error(
            None, INVALID_REQUEST, f"Unsupported protocol version {version}.", 400
        )
    who = request.app.state.auth.authenticate(request.headers.get("authorization"))
    request.state.principal = who.name
    try:
        msg = json.loads(await request.body())
    except (ValueError, UnicodeDecodeError):
        return _rpc_error(None, PARSE_ERROR, "Parse error.", 400)
    if not isinstance(msg, dict) or msg.get("jsonrpc") != "2.0":
        return _rpc_error(
            None, INVALID_REQUEST, "Expected one JSON-RPC 2.0 message.", 400
        )
    method = msg.get("method")
    if "id" not in msg:
        return Response(status_code=202)
    rid = msg["id"]
    if not isinstance(method, str):
        return _rpc_error(rid, INVALID_REQUEST, "method is required.")
    params = msg.get("params")
    try:
        if method == "initialize":
            asked = (params or {}).get("protocolVersion")
            return _result(
                rid,
                {
                    "protocolVersion": asked
                    if asked in PROTOCOL_VERSIONS
                    else PROTOCOL_VERSIONS[0],
                    "capabilities": {"tools": {"listChanged": False}},
                    "serverInfo": {
                        "name": SERVER_NAME,
                        "title": "OpenPlayX yt-dlp",
                        "version": __version__,
                    },
                    "instructions": INSTRUCTIONS,
                },
            )
        if method == "ping":
            return _result(rid, {})
        if method == "tools/list":
            return _result(rid, {"tools": [t.describe() for t in _visible(who)]})
        if method == "tools/call":
            return _result(rid, await _call(request, who, params))
    except _ParamsError as exc:
        return _rpc_error(rid, INVALID_PARAMS, str(exc))
    return _rpc_error(rid, METHOD_NOT_FOUND, f"Method not found: {method}")


@router.api_route("/mcp", methods=["GET", "DELETE"], include_in_schema=False)
async def mcp_other() -> Response:
    return Response(status_code=405, headers={"Allow": "POST"})
