"""Service error vocabulary: every failure has a stable machine code."""

from __future__ import annotations


class ApiError(Exception):
    def __init__(
        self,
        code: str,
        status: int,
        message: str,
        *,
        retryable: bool = False,
        headers: dict[str, str] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.status = status
        self.message = message
        self.retryable = retryable
        self.headers = headers or {}

    def body(self) -> dict:
        return {
            "error": {
                "code": self.code,
                "message": self.message,
                "retryable": self.retryable,
            }
        }


def invalid_request(message: str) -> ApiError:
    return ApiError("invalid_request", 400, message)


def unauthorized() -> ApiError:
    return ApiError(
        "unauthorized",
        401,
        "A valid bearer token is required.",
        headers={"WWW-Authenticate": "Bearer"},
    )


def forbidden(scope: str) -> ApiError:
    return ApiError("forbidden", 403, f"The token lacks the {scope!r} scope.")


def rate_limited(retry_after: int) -> ApiError:
    return ApiError(
        "rate_limited",
        429,
        "Too many requests for this token.",
        retryable=True,
        headers={"Retry-After": str(retry_after)},
    )


def url_blocked(message: str = "The URL is not permitted by the service.") -> ApiError:
    return ApiError("url_blocked", 400, message)


def unsupported_url(message: str = "No extractor supports this URL.") -> ApiError:
    return ApiError("unsupported_url", 422, message)


def extraction_failed(message: str) -> ApiError:
    return ApiError("extraction_failed", 502, message, retryable=True)


def timeout() -> ApiError:
    return ApiError(
        "upstream_timeout", 504, "The site did not answer in time.", retryable=True
    )


def playlist_not_supported(count: int | None) -> ApiError:
    n = f" ({count} entries)" if count else ""
    return ApiError(
        "playlist_not_supported",
        422,
        f"The URL is a playlist{n}; start one download per entry URL "
        "(get_media_info lists them).",
    )


def live_not_supported() -> ApiError:
    return ApiError(
        "live_not_supported", 422, "Live or upcoming streams cannot be downloaded."
    )


def too_long(duration: int, limit: int) -> ApiError:
    return ApiError(
        "too_long",
        422,
        f"Duration {duration}s exceeds the service limit of {limit}s.",
    )


def too_large(estimated: int, limit: int) -> ApiError:
    return ApiError(
        "too_large",
        422,
        f"Estimated size {estimated} bytes exceeds the per-download limit of "
        f"{limit} bytes; try a lower quality preset.",
    )


def insufficient_space(free: int, needed: int) -> ApiError:
    return ApiError(
        "insufficient_space",
        507,
        f"Not enough free space on the server ({free} bytes free, "
        f"{needed} bytes needed including the reserve).",
    )


def queue_full(limit: int) -> ApiError:
    return ApiError(
        "queue_full",
        429,
        f"{limit} downloads are already waiting or running.",
        retryable=True,
    )


def job_not_found() -> ApiError:
    return ApiError("not_found", 404, "Unknown download job.")


def job_active() -> ApiError:
    return ApiError(
        "job_active", 409, "The download is still running; cancel it first."
    )
