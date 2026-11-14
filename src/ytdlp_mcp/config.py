"""config.json / secrets.json loading.

config.json holds no credentials; secrets.json holds API token digests and
proxy URLs and is never committed. Paths, limits and network policy are an
administrator decision; nothing here is reachable through the API.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

Scope = Literal["read", "download"]

GIB = 1024**3


class _Strict(BaseModel):
    model_config = ConfigDict(extra="forbid")


class ServerConfig(_Strict):
    host: str = "127.0.0.1"
    port: int = Field(8800, ge=1, le=65535)
    log_level: Literal["DEBUG", "INFO", "WARNING", "ERROR"] = "INFO"


class StorageConfig(_Strict):
    database: str = "data/yt-dlp.sqlite3"
    # Finished files land here; partial files under <root>/.incomplete/<job>.
    download_root: str = "data/downloads"
    # yt-dlp cache (signature/player data).
    cache_dir: str = "data/cache"


class LimitsConfig(_Strict):
    max_concurrent_downloads: int = Field(1, ge=1, le=8)
    # Jobs waiting or running at once; further start_download calls fail.
    max_active_jobs: int = Field(10, ge=1, le=200)
    # Per job, all streams and subtitles together.
    max_task_bytes: int = Field(4 * GIB, ge=1024 * 1024)
    # Never let downloads push free space on download_root below this.
    min_free_bytes: int = Field(8 * GIB, ge=0)
    max_duration_seconds: int = Field(4 * 3600, ge=1)
    info_timeout_seconds: float = Field(45, gt=0, le=55)
    requests_per_minute: int = Field(120, ge=1, le=10000)
    max_subtitle_languages: int = Field(5, ge=0, le=20)


class NetworkConfig(_Strict):
    # LAN test fixtures only; the default accepts public addresses only.
    allow_private_addresses: bool = False
    # Empty: every extractor yt-dlp has. Otherwise extractor keys, e.g.
    # ["Youtube", "Vimeo", "BiliBili"] (case-insensitive).
    allowed_extractors: list[str] = Field(default_factory=list)
    # The generic extractor fetches arbitrary pages; off makes the service
    # accept supported sites only.
    allow_generic_extractor: bool = True
    # Key into secrets.json "proxies"; the URL may carry credentials.
    proxy_ref: str | None = None
    socket_timeout_seconds: float = Field(20, gt=0, le=120)
    rate_limit_bytes_per_second: int | None = Field(None, ge=1024)


class Config(_Strict):
    schema_version: Literal[1] = 1
    server: ServerConfig = Field(default_factory=ServerConfig)
    storage: StorageConfig = Field(default_factory=StorageConfig)
    limits: LimitsConfig = Field(default_factory=LimitsConfig)
    network: NetworkConfig = Field(default_factory=NetworkConfig)


class TokenEntry(_Strict):
    name: str = Field(pattern=r"^[A-Za-z0-9_.-]{1,64}$")
    # Either the plain token or its lowercase hex SHA-256.
    token: str | None = Field(None, min_length=24, max_length=512)
    token_sha256: str | None = Field(None, pattern=r"^[0-9a-f]{64}$")
    scopes: list[Scope] = Field(default_factory=lambda: ["read"])

    @model_validator(mode="after")
    def _one(self) -> TokenEntry:
        if (self.token is None) == (self.token_sha256 is None):
            raise ValueError("set exactly one of token / token_sha256")
        return self

    @property
    def digest(self) -> str:
        if self.token_sha256:
            return self.token_sha256
        return hashlib.sha256(self.token.encode()).hexdigest()


class Secrets(_Strict):
    tokens: list[TokenEntry] = Field(default_factory=list)
    proxies: dict[str, str] = Field(default_factory=dict)

    @model_validator(mode="after")
    def _unique(self) -> Secrets:
        names = [t.name for t in self.tokens]
        if len(names) != len(set(names)):
            raise ValueError("token names must be unique")
        return self


def load_config(path: str | os.PathLike | None) -> Config:
    if path is None:
        return Config()
    return Config.model_validate(json.loads(Path(path).read_text(encoding="utf-8")))


def load_secrets(path: str | os.PathLike | None) -> Secrets:
    if path is None:
        return Secrets()
    return Secrets.model_validate(json.loads(Path(path).read_text(encoding="utf-8")))


def resolve_proxy(config: Config, secrets: Secrets) -> str | None:
    ref = config.network.proxy_ref
    if ref is None:
        return None
    if ref not in secrets.proxies:
        raise ValueError(f"proxy_ref {ref!r} is not defined in secrets")
    return secrets.proxies[ref]
