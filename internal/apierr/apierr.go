// Package apierr is the service error vocabulary: every failure has a
// stable machine code, an HTTP status and a retryable flag.
package apierr

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/openplayx/mcp-yt-dlp/internal/ojson"
)

type Error struct {
	Code      string
	Status    int
	Message   string
	Retryable bool
	Headers   map[string]string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Body is the JSON error object: {"error": {code, message, retryable}}.
func (e *Error) Body() *ojson.Object {
	return ojson.New("error", ojson.New("code", e.Code, "message", e.Message, "retryable", e.Retryable))
}

func New(code string, status int, message string) *Error {
	return &Error{Code: code, Status: status, Message: message}
}

func InvalidRequest(msg string) *Error { return New("invalid_request", http.StatusBadRequest, msg) }

func Unauthorized() *Error {
	e := New("unauthorized", http.StatusUnauthorized, "A valid bearer token is required.")
	e.Headers = map[string]string{"WWW-Authenticate": "Bearer"}
	return e
}

func Forbidden(msg string) *Error { return New("forbidden", http.StatusForbidden, msg) }

func RateLimited(retryAfter int) *Error {
	e := New("rate_limited", http.StatusTooManyRequests, "Too many requests for this token.")
	e.Retryable = true
	e.Headers = map[string]string{"Retry-After": strconv.Itoa(retryAfter)}
	return e
}

func URLBlocked(msg string) *Error {
	if msg == "" {
		msg = "The URL is not permitted by the service."
	}
	return New("url_blocked", http.StatusBadRequest, msg)
}

func UnsupportedURL(msg string) *Error {
	if msg == "" {
		msg = "No extractor supports this URL."
	}
	return New("unsupported_url", http.StatusUnprocessableEntity, msg)
}

func ExtractionFailed(msg string) *Error {
	e := New("extraction_failed", http.StatusBadGateway, msg)
	e.Retryable = true
	return e
}

func Timeout() *Error {
	e := New("upstream_timeout", http.StatusGatewayTimeout, "The site did not answer in time.")
	e.Retryable = true
	return e
}

func PlaylistNotSupported(count int) *Error {
	n := ""
	if count > 0 {
		n = fmt.Sprintf(" (%d entries)", count)
	}
	return New("playlist_not_supported", http.StatusUnprocessableEntity,
		"The URL is a playlist"+n+"; start one download per entry URL (get_media_info lists them).")
}

func LiveNotSupported() *Error {
	return New("live_not_supported", http.StatusUnprocessableEntity, "Live or upcoming streams cannot be downloaded.")
}

func TooLong(duration, limit int64) *Error {
	return New("too_long", http.StatusUnprocessableEntity,
		fmt.Sprintf("Duration %ds exceeds the service limit of %ds.", duration, limit))
}

func TooLarge(estimated, limit int64) *Error {
	return New("too_large", http.StatusUnprocessableEntity,
		fmt.Sprintf("Estimated size %d bytes exceeds the per-download limit of %d bytes; try a lower quality preset.", estimated, limit))
}

func InsufficientSpace(free, needed int64) *Error {
	return New("insufficient_space", http.StatusInsufficientStorage,
		fmt.Sprintf("Not enough free space on the server (%d bytes free, %d bytes needed including the reserve).", free, needed))
}

func QueueFull(limit int) *Error {
	e := New("queue_full", http.StatusTooManyRequests, fmt.Sprintf("%d downloads are already waiting or running.", limit))
	e.Retryable = true
	return e
}

func JobNotFound() *Error { return New("not_found", http.StatusNotFound, "Unknown download job.") }

func JobActive() *Error {
	return New("job_active", http.StatusConflict, "The download is still running; cancel it first.")
}

func Internal() *Error {
	e := New("internal_error", http.StatusInternalServerError, "Unexpected server error.")
	e.Retryable = true
	return e
}
