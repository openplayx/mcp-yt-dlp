"""Bearer token authentication with per-token scopes and request budget."""

from __future__ import annotations

import hashlib
import hmac
import math
import threading
import time
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass

from . import errors
from .config import Secrets


@dataclass(frozen=True)
class Principal:
    name: str
    scopes: frozenset[str]


class Authenticator:
    def __init__(
        self,
        secrets: Secrets,
        requests_per_minute: int,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._tokens = [
            (t.digest, Principal(t.name, frozenset(t.scopes))) for t in secrets.tokens
        ]
        self._rpm = requests_per_minute
        self._clock = clock
        self._hits: dict[str, deque[float]] = {}
        self._lock = threading.Lock()

    def authenticate(self, authorization: str | None) -> Principal:
        if not authorization:
            raise errors.unauthorized()
        scheme, _, token = authorization.partition(" ")
        token = token.strip()
        if scheme.lower() != "bearer" or not token or len(token) > 512:
            raise errors.unauthorized()
        digest = hashlib.sha256(token.encode()).hexdigest()
        found = None
        for stored, principal in self._tokens:
            # Compare every entry: timing does not reveal which one matched.
            if hmac.compare_digest(stored, digest):
                found = principal
        if found is None:
            raise errors.unauthorized()
        self._budget(found.name)
        return found

    def _budget(self, name: str) -> None:
        now = self._clock()
        with self._lock:
            hits = self._hits.setdefault(name, deque())
            while hits and hits[0] <= now - 60:
                hits.popleft()
            if len(hits) >= self._rpm:
                raise errors.rate_limited(max(1, math.ceil(hits[0] + 60 - now)))
            hits.append(now)
