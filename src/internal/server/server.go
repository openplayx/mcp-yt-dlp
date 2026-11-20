// Package server serves /healthz, /readyz, authenticated files and MCP.
//
// MCP (Streamable HTTP, POST /mcp) uses the same deliberately small subset
// as the other OpenPlayX MCP services:
//
//   - JSON-RPC 2.0 single messages over POST; application/json responses
//     (no SSE stream, no server-initiated messages).
//   - Stateless: no Mcp-Session-Id; every request carries the bearer token.
//   - initialize, ping, tools/list, tools/call and notifications.
//   - GET/DELETE answer 405.
//
// Business failures are tool results with isError=true and a stable error
// code, so the model can see and explain them. Tools that change server
// state are not marked read-only; OpenPlayX asks the user to confirm those.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/openplayx/mcp-yt-dlp/src/internal/apierr"
	"github.com/openplayx/mcp-yt-dlp/src/internal/auth"
	"github.com/openplayx/mcp-yt-dlp/src/internal/media"
	"github.com/openplayx/mcp-yt-dlp/src/internal/ojson"
	"github.com/openplayx/mcp-yt-dlp/src/internal/service"
)

var ProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	ServerName     = "openplayx-yt-dlp"
	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
	invalidParams  = -32602
	maxBody        = 1 << 20
	instructions   = "Downloads online videos (YouTube and the other sites yt-dlp supports) to " +
		"the user's own server. get_media_info inspects a URL without downloading. " +
		"start_download queues one video at a chosen quality preset and returns a " +
		"job_id; downloads run in the background, so poll get_download for " +
		"progress. Completed files include download_path and bytes; clients can fetch " +
		"them with authenticated HTTP GET/HEAD and Range, without MCP resources/read. " +
		"Playlists and live streams are not downloaded as a whole. The " +
		"user must have the right to download the content."
)

type Server struct {
	svc  *service.Service
	auth *auth.Authenticator
	log  *slog.Logger
	mux  *http.ServeMux
	// LookPath finds ffmpeg and the JS runtime for /readyz (tests replace it).
	LookPath func(string) (string, error)
}

func New(svc *service.Service, a *auth.Authenticator, log *slog.Logger) *Server {
	s := &Server{svc: svc, auth: a, log: log, mux: http.NewServeMux(), LookPath: exec.LookPath}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /files/{job_id}/{file_index}", s.file)
	s.mux.HandleFunc("POST /mcp", s.mcp)
	s.mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apierr.New("not_found", http.StatusNotFound, "No such route.").Body())
	})
	return s
}

type reqState struct {
	principal, tool, errorCode string
}

type stateKey struct{}

func state(r *http.Request) *reqState {
	if st, ok := r.Context().Value(stateKey{}).(*reqState); ok {
		return st
	}
	return &reqState{}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	st := &reqState{principal: "-", tool: "-", errorCode: "-"}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	s.mux.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	// Never log bodies (URLs, tokens); tool name and error code only.
	if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
		s.log.Info("request", "id", hex.EncodeToString(b), "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(started).Milliseconds(),
			"principal", st.principal, "tool", st.tool, "error", st.errorCode)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, r *http.Request, e *apierr.Error) {
	state(r).errorCode = e.Code
	for k, v := range e.Headers {
		w.Header().Set(k, v)
	}
	writeJSON(w, e.Status, e.Body())
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ojson.New("status", "ok", "version", s.svc.Version))
}

// GET patterns also handle HEAD. ServeContent supplies Range, If-Range,
// Content-Length and 416 responses without buffering the media in memory.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		writeAPIError(w, r, apierr.Forbidden("Origin not allowed."))
		return
	}
	who, aerr := s.auth.Authenticate(r.Header.Get("Authorization"))
	if aerr != nil {
		writeAPIError(w, r, aerr)
		return
	}
	state(r).principal = who.Name
	if !who.Has("read") {
		writeAPIError(w, r, apierr.Forbidden("File downloads require read permission."))
		return
	}
	id := r.PathValue("job_id")
	index, err := strconv.Atoi(r.PathValue("file_index"))
	if !jobIDPat.MatchString(id) || len(id) > 32 || err != nil || index < 0 {
		writeAPIError(w, r, apierr.InvalidRequest("Expected a job_id and a non-negative file_index."))
		return
	}
	f, aerr := s.svc.Jobs.OpenFile(id, index)
	if aerr != nil {
		writeAPIError(w, r, aerr)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeAPIError(w, r, apierr.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+strings.ReplaceAll(url.QueryEscape(info.Name()), "+", "%20"))
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	// File transfers may legitimately outlive the MCP server's two-minute
	// write deadline. Preserve request cancellation, but remove the absolute
	// deadline for this authenticated streaming response only.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".readyz-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	has := func(bin string) bool { _, err := s.LookPath(bin); return err == nil }
	checks := ojson.New(
		"download_root_writable", writable(s.svc.Jobs.Root),
		"yt_dlp", s.svc.Engine.Version() != "unknown",
		"ffmpeg", has("ffmpeg"),
		"js_runtime", has("deno"),
	)
	ok := true
	for _, k := range []string{"download_root_writable", "yt_dlp", "ffmpeg", "js_runtime"} {
		v, _ := checks.Get(k)
		ok = ok && v.(bool)
	}
	status, code := "ready", http.StatusOK
	if !ok {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	writeJSON(w, code, ojson.New("status", status, "checks", checks))
}

// ---- tools -----------------------------------------------------------------

type tool struct {
	name, title, description, scope string
	schema                          *ojson.Object
	run                             func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error)
	readOnly, destructive           bool
	notIdempotent                   bool
}

func (t tool) describe() *ojson.Object {
	return ojson.New(
		"name", t.name, "title", t.title, "description", t.description, "inputSchema", t.schema,
		"annotations", ojson.New("title", t.title, "readOnlyHint", t.readOnly,
			"destructiveHint", t.destructive, "idempotentHint", !t.notIdempotent, "openWorldHint", true),
	)
}

func object(props *ojson.Object, required ...string) *ojson.Object {
	o := ojson.New("type", "object", "properties", props, "additionalProperties", false)
	if len(required) > 0 {
		o.Set("required", required)
	}
	return o
}

var (
	urlProp  = ojson.New("type", "string", "maxLength", 2048, "description", "Page URL of one video.")
	jobProp  = ojson.New("type", "string", "maxLength", 32, "description", "job_id from start_download.")
	states   = []string{"active", "queued", "running", "completed", "failed", "canceled", "interrupted"}
	jobIDPat = mustRe(`^[0-9a-f]+$`)
)

func jobID(a *args) string {
	id := a.str("job_id", true, "", 1, 32)
	a.pattern("job_id", id, jobIDPat)
	return id
}

func langs(raw []string) ([]string, *apierr.Error) {
	out := []string{}
	for _, s := range raw {
		s = strings.TrimSpace(s)
		bad := s == "" || len([]rune(s)) > 20 || strings.IndexFunc(s, func(c rune) bool {
			return !(unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_')
		}) >= 0
		if bad {
			b, _ := json.Marshal(s)
			return nil, apierr.InvalidRequest("invalid subtitle language " + string(b))
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

var tools = []tool{
	{
		name: "get_status", title: "Download service status", scope: "read", readOnly: true,
		description: "Free space on the server, size/duration limits, quality presets and job counts. Read-only.",
		schema:      object(&ojson.Object{}),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.Status(), nil
		},
	},
	{
		name: "get_media_info", title: "Inspect media URL", scope: "read", readOnly: true,
		description: "Reads title, uploader, duration, available resolutions, subtitle " +
			"languages and estimated size per preset for a video URL; for a " +
			"playlist, lists its entries (each entry URL can be downloaded). Never downloads.",
		schema: object(ojson.New("url", urlProp), "url"),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.Info(ctx, a.vals["url"].(string))
		},
	},
	{
		name: "start_download", title: "Download video to server", scope: "download", notIdempotent: true,
		description: "Queues a download of one video to the server and returns a job " +
			"(job_id, state). Checks size, duration and free space first. If the " +
			"same URL and preset is already queued or running, returns that job " +
			"instead. Poll get_download for progress and the resulting files.",
		schema: object(ojson.New(
			"url", urlProp,
			"preset", ojson.New("type", "string", "enum", media.PresetKeys(), "default", media.DefaultPreset,
				"description", "Quality: best, 2160p/1080p/720p/480p caps, or audio (m4a)."),
			"subtitles", ojson.New("type", "array", "items", ojson.New("type", "string", "maxLength", 20),
				"maxItems", 20, "default", []string{},
				"description", `Subtitle language codes to save as .srt next to the video, e.g. ["en", "zh-Hans"]. Uploaded subtitles only.`),
		), "url"),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			l, err := langs(a.vals["subtitles"].([]string))
			if err != nil {
				return nil, err
			}
			return s.svc.Start(ctx, who.Name, a.vals["url"].(string), a.vals["preset"].(string), l)
		},
	},
	{
		name: "list_downloads", title: "List downloads", scope: "read", readOnly: true,
		description: "Recent download jobs, newest first. Read-only.",
		schema: object(ojson.New(
			"state", ojson.New("type", "string", "enum", states, "description", "Filter; 'active' = queued or running."),
			"limit", ojson.New("type", "integer", "minimum", 1, "maximum", 100, "default", 20),
		)),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.List(a.vals["state"].(string), a.vals["limit"].(int)), nil
		},
	},
	{
		name: "get_download", title: "Download progress", scope: "read", readOnly: true,
		description: "State, progress, resulting files (paths relative to the download " +
			"folder, sizes) and error of one job. During downloads, progress.scope is " +
			"current_stream and stream_index identifies the stream (1-based); percent, " +
			"downloaded_bytes, total_bytes and eta_seconds all refer to that stream. " +
			"overall_downloaded_bytes is cumulative. Show approximate sizes when " +
			"total_bytes_estimated is true; estimates may change. ETA is estimated " +
			"from remaining bytes / speed_bps. Null means unknown. estimated_bytes " +
			"is the initial job estimate, not the progress denominator. Read-only.",
		schema: object(ojson.New("job_id", jobProp), "job_id"),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.Get(a.vals["job_id"].(string))
		},
	},
	{
		name: "cancel_download", title: "Cancel download", scope: "download",
		description: "Stops a queued or running job and discards its partial files. Finished jobs are left unchanged.",
		schema:      object(ojson.New("job_id", jobProp), "job_id"),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.Cancel(a.vals["job_id"].(string))
		},
	},
	{
		name: "remove_download", title: "Remove download", scope: "download", destructive: true,
		description: "Removes a finished job from the list. With delete_files=true also " +
			"deletes its downloaded files from the server (cannot be undone).",
		schema: object(ojson.New("job_id", jobProp, "delete_files", ojson.New("type", "boolean", "default", false)), "job_id"),
		run: func(ctx context.Context, s *Server, who auth.Principal, a *args) (any, *apierr.Error) {
			return s.svc.Remove(a.vals["job_id"].(string), a.vals["delete_files"].(bool))
		},
	},
}

// validate reads the arguments a tool accepts into a.vals.
func validate(name string, a *args) string {
	a.vals = map[string]any{}
	switch name {
	case "get_media_info":
		a.vals["url"] = a.str("url", true, "", 1, 2048)
	case "start_download":
		a.vals["url"] = a.str("url", true, "", 1, 2048)
		a.vals["preset"] = a.enum("preset", media.DefaultPreset, media.PresetKeys(), false)
		a.vals["subtitles"] = a.strList("subtitles", 20, 0)
	case "list_downloads":
		a.vals["state"] = a.enum("state", "", states, true)
		a.vals["limit"] = a.integer("limit", 20, 1, 100)
	case "get_download", "cancel_download":
		a.vals["job_id"] = jobID(a)
	case "remove_download":
		a.vals["job_id"] = jobID(a)
		a.vals["delete_files"] = a.boolean("delete_files", false)
	}
	return a.done()
}

func toolResult(payload any, isError bool) *ojson.Object {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	return ojson.New(
		"content", []any{ojson.New("type", "text", "text", strings.TrimRight(buf.String(), "\n"))},
		"structuredContent", payload,
		"isError", isError,
	)
}

// ---- transport ---------------------------------------------------------------

func rpcError(id any, code int, msg string) *ojson.Object {
	return ojson.New("jsonrpc", "2.0", "id", id, "error", ojson.New("code", code, "message", msg))
}

func result(id any, res any) *ojson.Object {
	return ojson.New("jsonrpc", "2.0", "id", id, "result", res)
}

func originAllowed(r *http.Request) bool {
	// DNS-rebinding guard: a browser page may only call us from our own origin.
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if _, rest, ok := strings.Cut(origin, "://"); ok {
		origin = rest
	}
	return strings.TrimRight(origin, "/") == r.Host
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		writeAPIError(w, r, apierr.Forbidden("Origin not allowed."))
		return
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		accept = "*/*"
	}
	if !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	if v := r.Header.Get("Mcp-Protocol-Version"); v != "" && !slices.Contains(ProtocolVersions, v) {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, invalidRequest, "Unsupported protocol version "+v+"."))
		return
	}
	who, aerr := s.auth.Authenticate(r.Header.Get("Authorization"))
	if aerr != nil {
		writeAPIError(w, r, aerr)
		return
	}
	state(r).principal = who.Name
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcError(nil, invalidRequest, "Message too large."))
		return
	}
	var raw any
	if json.Unmarshal(body, &raw) != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, parseError, "Parse error."))
		return
	}
	var msg map[string]json.RawMessage
	if _, isObj := raw.(map[string]any); !isObj || json.Unmarshal(body, &msg) != nil ||
		string(msg["jsonrpc"]) != `"2.0"` {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, invalidRequest, "Expected one JSON-RPC 2.0 message."))
		return
	}
	idRaw, hasID := msg["id"]
	if !hasID {
		// Notifications and client responses: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var id any = idRaw
	if string(bytes.TrimSpace(idRaw)) == "null" {
		id = nil
	}
	var method string
	if json.Unmarshal(msg["method"], &method) != nil {
		writeJSON(w, http.StatusOK, rpcError(id, invalidRequest, "method is required."))
		return
	}
	params := msg["params"]
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		version := ProtocolVersions[0]
		if slices.Contains(ProtocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		writeJSON(w, http.StatusOK, result(id, ojson.New(
			"protocolVersion", version,
			"capabilities", ojson.New("tools", ojson.New("listChanged", false)),
			"serverInfo", ojson.New("name", ServerName, "title", "OpenPlayX yt-dlp", "version", s.svc.Version),
			"instructions", instructions,
		)))
	case "ping":
		writeJSON(w, http.StatusOK, result(id, &ojson.Object{}))
	case "tools/list":
		list := []any{}
		for _, t := range tools {
			if who.Has(t.scope) {
				list = append(list, t.describe())
			}
		}
		writeJSON(w, http.StatusOK, result(id, ojson.New("tools", list)))
	case "tools/call":
		res, code, emsg := s.call(r, who, params)
		if code != 0 {
			writeJSON(w, http.StatusOK, rpcError(id, code, emsg))
			return
		}
		writeJSON(w, http.StatusOK, result(id, res))
	default:
		writeJSON(w, http.StatusOK, rpcError(id, methodNotFound, "Method not found: "+method))
	}
}

func (s *Server) call(r *http.Request, who auth.Principal, params json.RawMessage) (any, int, string) {
	var p map[string]json.RawMessage
	var name string
	if json.Unmarshal(params, &p) != nil || p == nil || json.Unmarshal(p["name"], &name) != nil {
		return nil, invalidParams, "params.name is required."
	}
	i := slices.IndexFunc(tools, func(t tool) bool { return t.name == name })
	if i < 0 || !who.Has(tools[i].scope) {
		return nil, invalidParams, "Unknown tool: " + name
	}
	t := tools[i]
	a, ok := parseArgs(p["arguments"])
	if !ok {
		return nil, invalidParams, "params.arguments must be an object."
	}
	if msg := validate(name, a); msg != "" {
		return toolResult(apierr.InvalidRequest(msg).Body(), true), 0, ""
	}
	state(r).tool = name
	out, aerr := t.run(r.Context(), s, who, a)
	if aerr != nil {
		state(r).errorCode = aerr.Code
		return toolResult(aerr.Body(), true), 0, ""
	}
	return toolResult(out, false), 0, ""
}
