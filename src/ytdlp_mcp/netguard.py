"""Checks on a caller-supplied media URL before yt-dlp sees it.

Only absolute http(s) URLs without credentials, whose host resolves to public
addresses (unless the administrator enabled private addresses for LAN tests).

Residual risk: yt-dlp follows redirects and extractors fetch further URLs
(APIs, CDNs, manifests) that this check never sees, and DNS can change
between the check and the connection. The generic extractor can be turned
off (network.allow_generic_extractor) to limit fetching to known sites.
"""

from __future__ import annotations

import ipaddress
import socket
from collections.abc import Callable
from urllib.parse import urlsplit

from . import errors

Resolver = Callable[[str, int], list[str]]

MAX_URL_LENGTH = 2048


def system_resolver(host: str, port: int) -> list[str]:
    infos = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    return sorted({info[4][0] for info in infos})


def _address_ok(addr: str) -> bool:
    ip = ipaddress.ip_address(addr.split("%", 1)[0])
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
        ip = ip.ipv4_mapped
    return ip.is_global and not ip.is_multicast


def check_url(url: str, *, allow_private: bool, resolver: Resolver) -> str:
    """Returns the normalised URL or raises ``url_blocked``."""
    url = url.strip()
    if not url or len(url) > MAX_URL_LENGTH or any(c.isspace() for c in url):
        raise errors.url_blocked("The URL is empty, too long or contains spaces.")
    try:
        parts = urlsplit(url)
        port = parts.port
    except ValueError as exc:
        raise errors.url_blocked("The URL is malformed.") from exc
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise errors.url_blocked("Only absolute http(s) URLs are accepted.")
    if parts.username or parts.password:
        raise errors.url_blocked("URLs with credentials are not accepted.")
    if allow_private:
        return url
    host = parts.hostname.lower()
    port = port or (443 if parts.scheme == "https" else 80)
    try:
        literal = ipaddress.ip_address(host.strip("[]"))
    except ValueError:
        literal = None
    if literal is not None:
        addresses = [str(literal)]
    else:
        if host == "localhost" or host.endswith((".localhost", ".local", ".lan")):
            raise errors.url_blocked("Local addresses are not accepted.")
        try:
            addresses = resolver(host, port)
        except OSError as exc:
            raise errors.url_blocked("The URL host does not resolve.") from exc
    if not addresses or not all(_address_ok(a) for a in addresses):
        raise errors.url_blocked("Private or local addresses are not accepted.")
    return url
