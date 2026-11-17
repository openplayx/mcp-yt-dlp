package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openplayx/mcp-yt-dlp/src/internal/auth"
	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
	"github.com/openplayx/mcp-yt-dlp/src/internal/engine"
	"github.com/openplayx/mcp-yt-dlp/src/internal/jobs"
	"github.com/openplayx/mcp-yt-dlp/src/internal/media"
	"github.com/openplayx/mcp-yt-dlp/src/internal/service"
)

var (
	token     = strings.Repeat("t", 32)
	readToken = strings.Repeat("r", 32)
)

const gib = int64(1) << 30

func video(over map[string]any) media.Info {
	info := media.Info{
		"id": "abc123", "title": "A video", "extractor_key": "Youtube",
		"webpage_url": "https://www.youtube.com/watch?v=abc123",
		"duration":    60.0, "live_status": "not_live",
		"formats": []any{
			map[string]any{"format_id": "v1", "vcodec": "avc1", "acodec": "none", "height": 1080.0, "filesize": 50_000_000.0, "tbr": 4000.0},
			map[string]any{"format_id": "v2", "vcodec": "avc1", "acodec": "none", "height": 720.0, "filesize": 20_000_000.0, "tbr": 2000.0},
			map[string]any{"format_id": "a1", "vcodec": "none", "acodec": "mp4a", "abr": 128.0, "filesize": 1_000_000.0},
		},
		"subtitles": map[string]any{"en": []any{map[string]any{}}},
	}
	for k, v := range over {
		info[k] = v
	}
	return info
}

type fakeEngine struct {
	mu      sync.Mutex
	info    media.Info
	err     error
	chunks  []int64
	gate    chan struct{} // nil: no gate; closed: open
	started chan struct{}
	once    sync.Once
	specs   []engine.DownloadSpec
}

func newFake() *fakeEngine {
	return &fakeEngine{info: video(nil), chunks: []int64{1000, 2000}, started: make(chan struct{})}
}

func (f *fakeEngine) set(info media.Info, err error) {
	f.mu.Lock()
	f.info, f.err = info, err
	f.mu.Unlock()
}

func (f *fakeEngine) Version() string { return "test" }

func (f *fakeEngine) Probe(ctx context.Context, url string) (media.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, f.err
}

func (f *fakeEngine) Download(ctx context.Context, spec engine.DownloadSpec, progress engine.ProgressFunc) ([]string, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	chunks, gate := f.chunks, f.gate
	f.mu.Unlock()
	_ = os.MkdirAll(spec.Temp, 0o755)
	_ = os.WriteFile(filepath.Join(spec.Temp, "part"), []byte("x"), 0o644)
	f.once.Do(func() { close(f.started) })
	var total, done int64
	for _, c := range chunks {
		total += c
	}
	for _, c := range chunks {
		done += c
		d := done
		if err := progress(engine.Progress{Phase: "downloading", Filename: "f.part", DownloadedBytes: &d, TotalBytes: &total}); err != nil {
			return nil, err
		}
		if gate != nil {
			for {
				select {
				case <-gate:
				case <-time.After(10 * time.Millisecond):
					if err := progress(engine.Progress{Phase: "downloading", Filename: "f.part", DownloadedBytes: &d}); err != nil {
						return nil, err
					}
					continue
				}
				break
			}
		}
	}
	out := filepath.Join(spec.Home, "Youtube", "A video [abc123].mp4")
	_ = os.MkdirAll(filepath.Dir(out), 0o755)
	_ = os.WriteFile(out, bytes.Repeat([]byte("v"), int(done)), 0o644)
	sub := filepath.Join(spec.Home, "Youtube", "A video [abc123].en.srt")
	_ = os.WriteFile(sub, []byte("1\n"), 0o644)
	return engine.Collect([]string{out}, spec.Home), nil
}

func publicResolver(ctx context.Context, host string, port int) ([]string, error) {
	if host == "lan.example" {
		return []string{"192.168.1.5"}, nil
	}
	return []string{"93.184.216.34"}, nil
}

type env struct {
	t      *testing.T
	dir    string
	engine *fakeEngine
	free   atomic.Int64
	jm     *jobs.Manager
	store  *jobs.Store
	srv    *httptest.Server
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, dir: t.TempDir(), engine: newFake()}
	e.free.Store(100 * gib)
	return e
}

func (e *env) start(limits map[string]any) {
	e.t.Helper()
	lim := map[string]any{"min_free_bytes": gib}
	for k, v := range limits {
		lim[k] = v
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": map[string]any{
			"database":      filepath.Join(e.dir, "db.sqlite3"),
			"download_root": filepath.Join(e.dir, "dl"),
			"cache_dir":     filepath.Join(e.dir, "cache"),
		},
		"limits": lim,
	})
	cfg, err := config.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	secrets, err := config.ParseSecrets([]byte(`{"tokens": [
		{"name": "app", "token": "` + token + `", "scopes": ["read", "download"]},
		{"name": "ro", "token": "` + readToken + `", "scopes": ["read"]}]}`))
	if err != nil {
		e.t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.store, err = jobs.OpenStore(cfg.Storage.Database)
	if err != nil {
		e.t.Fatal(err)
	}
	e.jm, err = jobs.NewManager(cfg, e.engine, e.store, func(string) (int64, error) { return e.free.Load(), nil }, log)
	if err != nil {
		e.t.Fatal(err)
	}
	svc := service.New(cfg, e.engine, e.jm, publicResolver, "test")
	s := New(svc, auth.New(secrets, cfg.Limits.RequestsPerMinute), log)
	s.LookPath = func(string) (string, error) { return "/bin/true", nil }
	e.srv = httptest.NewServer(s)
	e.t.Cleanup(e.stop)
}

func (e *env) stop() {
	if e.srv == nil {
		return
	}
	e.srv.Close()
	e.jm.Shutdown(5 * time.Second)
	e.store.Close()
	e.srv = nil
}

func (e *env) post(body any, tok string, headers ...string) *http.Response {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp", r)
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (e *env) rpc(method string, params any, tok string) map[string]any {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	return decode(e.t, e.post(body, tok))
}

func (e *env) call(name string, args map[string]any, tok ...string) (map[string]any, bool) {
	e.t.Helper()
	tk := token
	if len(tok) > 0 {
		tk = tok[0]
	}
	if args == nil {
		args = map[string]any{}
	}
	out := e.rpc("tools/call", map[string]any{"name": name, "arguments": args}, tk)
	res, ok := out["result"].(map[string]any)
	if !ok {
		e.t.Fatalf("no result: %v", out)
	}
	return res["structuredContent"].(map[string]any), res["isError"].(bool)
}

func errCode(out map[string]any) string {
	return out["error"].(map[string]any)["code"].(string)
}

func (e *env) waitState(id string, states ...string) map[string]any {
	e.t.Helper()
	if len(states) == 0 {
		states = []string{"completed", "failed", "canceled"}
	}
	var job map[string]any
	for range 300 {
		job, _ = e.call("get_download", map[string]any{"job_id": id})
		if slices.Contains(states, job["state"].(string)) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job never reached %v: %v", states, job)
	return nil
}

func names(list []any) []string {
	var out []string
	for _, t := range list {
		out = append(out, t.(map[string]any)["name"].(string))
	}
	sort.Strings(out)
	return out
}

func TestHandshakeAndTools(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	res := e.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"}, token)["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "openplayx-yt-dlp" {
		t.Fatal(res)
	}
	list := e.rpc("tools/list", nil, token)["result"].(map[string]any)["tools"].([]any)
	got := names(list)
	want := []string{"cancel_download", "get_download", "get_media_info", "get_status", "list_downloads", "remove_download", "start_download"}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
	ann := map[string]map[string]any{}
	for _, x := range list {
		m := x.(map[string]any)
		ann[m["name"].(string)] = m["annotations"].(map[string]any)
	}
	if ann["get_media_info"]["readOnlyHint"] != true || ann["start_download"]["readOnlyHint"] != false ||
		ann["remove_download"]["destructiveHint"] != true {
		t.Fatal(ann)
	}
	ro := names(e.rpc("tools/list", nil, readToken)["result"].(map[string]any)["tools"].([]any))
	if !slices.Equal(ro, []string{"get_download", "get_media_info", "get_status", "list_downloads"}) {
		t.Fatal(ro)
	}
	out := e.rpc("tools/call", map[string]any{"name": "start_download", "arguments": map[string]any{"url": "https://x.com/v"}}, readToken)
	if out["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatal(out)
	}
}

func TestAuthAndTransport(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	if r := e.post(map[string]any{}, ""); r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatal(r.Status)
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, strings.Repeat("w", 32)); r.StatusCode != 401 {
		t.Fatal(r.Status)
	}
	if r, _ := http.Get(e.srv.URL + "/mcp"); r.StatusCode != 405 {
		t.Fatal(r.Status)
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "method": "notifications/x"}, token); r.StatusCode != 202 {
		t.Fatal(r.Status)
	}
	r, _ := http.Get(e.srv.URL + "/healthz")
	if decode(t, r)["status"] != "ok" {
		t.Fatal("healthz")
	}
	r, _ = http.Get(e.srv.URL + "/readyz")
	if r.StatusCode != 200 {
		t.Fatal(decode(t, r))
	}
	if r := e.post("{not json", token); r.StatusCode != 400 || decode(t, r)["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatal("parse error")
	}
	if r := e.post(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, token); r.StatusCode != 400 {
		t.Fatal("batch")
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, token, "Origin", "https://evil.example"); r.StatusCode != 403 {
		t.Fatal("origin")
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, token, "Accept", "text/html"); r.StatusCode != 406 {
		t.Fatal("accept")
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, token, "MCP-Protocol-Version", "1999-01-01"); r.StatusCode != 400 {
		t.Fatal("version")
	}
	if out := e.rpc("resources/list", nil, token); out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatal(out)
	}
}

func TestMediaInfo(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	out, isErr := e.call("get_media_info", map[string]any{"url": "https://www.youtube.com/watch?v=abc123"})
	if isErr || out["kind"] != "video" {
		t.Fatal(out)
	}
	if h := out["available_heights"].([]any); len(h) != 2 || h[0].(float64) != 1080 || h[1].(float64) != 720 {
		t.Fatal(h)
	}
	est := map[string]any{}
	for _, p := range out["presets"].([]any) {
		m := p.(map[string]any)
		est[m["preset"].(string)] = m["estimated_bytes"]
	}
	if est["1080p"].(float64) != 51_000_000 || est["720p"].(float64) != 21_000_000 || est["audio"].(float64) != 1_000_000 {
		t.Fatal(est)
	}
	e.engine.set(media.Info{"_type": "playlist", "title": "L", "playlist_count": 2.0,
		"entries": []any{map[string]any{"id": "1", "url": "https://y/1", "title": "one"}}}, nil)
	out, _ = e.call("get_media_info", map[string]any{"url": "https://www.youtube.com/playlist?list=x"})
	if out["kind"] != "playlist" || out["entry_count"].(float64) != 2 {
		t.Fatal(out)
	}
	e.engine.set(nil, &engine.Error{Code: "unsupported_url", Message: "Unsupported URL: x"})
	out, isErr = e.call("get_media_info", map[string]any{"url": "https://example.org/"})
	if !isErr || errCode(out) != "unsupported_url" {
		t.Fatal(out)
	}
	e.engine.set(nil, context.DeadlineExceeded)
	out, isErr = e.call("get_media_info", map[string]any{"url": "https://example.org/"})
	if !isErr || errCode(out) != "upstream_timeout" {
		t.Fatal(out)
	}
}

func TestBlockedURLs(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	for _, u := range []string{
		"file:///etc/passwd", "http://127.0.0.1/x", "http://lan.example/v",
		"http://localhost:8800/mcp", "https://user:pw@example.com/v", "ftp://x/y",
		"http://[::1]/x", "http://169.254.169.254/latest", "http://100.64.0.1/",
	} {
		out, isErr := e.call("start_download", map[string]any{"url": u})
		if !isErr || errCode(out) != "url_blocked" {
			t.Errorf("%s: %v", u, out)
		}
	}
}

func TestDownloadLifecycle(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	out, isErr := e.call("start_download", map[string]any{
		"url": "https://www.youtube.com/watch?v=abc123", "preset": "720p", "subtitles": []any{"en", "en"},
	})
	if isErr || out["created"] != true {
		t.Fatal(out)
	}
	id := out["job"].(map[string]any)["job_id"].(string)
	job := e.waitState(id)
	if job["state"] != "completed" {
		t.Fatal(job)
	}
	var paths []string
	for _, f := range job["files"].([]any) {
		paths = append(paths, f.(map[string]any)["path"].(string))
	}
	if !slices.Equal(paths, []string{"Youtube/A video [abc123].en.srt", "Youtube/A video [abc123].mp4"}) &&
		!slices.Equal(paths, []string{"Youtube/A video [abc123].mp4", "Youtube/A video [abc123].en.srt"}) {
		t.Fatal(paths)
	}
	p := job["progress"].(map[string]any)
	if job["estimated_bytes"].(float64) != 21_000_000 || p["phase"] != "completed" || p["percent"].(float64) != 100 {
		t.Fatal(job)
	}
	if _, ok := p["speed_bps"]; ok {
		t.Fatal("speed kept on a finished job")
	}
	if !slices.Equal(e.engine.specs[0].Subtitles, []string{"en"}) {
		t.Fatal(e.engine.specs[0].Subtitles)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "dl", ".incomplete", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp dir left behind")
	}
	listed, _ := e.call("list_downloads", map[string]any{"state": "completed"})
	if jl := listed["jobs"].([]any); len(jl) != 1 || jl[0].(map[string]any)["job_id"] != id {
		t.Fatal(listed)
	}
	status, _ := e.call("get_status", nil)
	if status["jobs"].(map[string]any)["completed"].(float64) != 1 || status["yt_dlp_version"] != "test" {
		t.Fatal(status)
	}
	out, isErr = e.call("remove_download", map[string]any{"job_id": id, "delete_files": true})
	if isErr || len(out["deleted_files"].([]any)) != 2 || out["freed_bytes"].(float64) != 3002 {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "dl", "Youtube")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty folder kept")
	}
	out, isErr = e.call("get_download", map[string]any{"job_id": id})
	if !isErr || errCode(out) != "not_found" {
		t.Fatal(out)
	}
}

func TestCancelRunningAndDedupe(t *testing.T) {
	e := newEnv(t)
	e.engine.gate = make(chan struct{})
	e.start(nil)
	args := map[string]any{"url": "https://www.youtube.com/watch?v=abc123"}
	out, _ := e.call("start_download", args)
	id := out["job"].(map[string]any)["job_id"].(string)
	select {
	case <-e.engine.started:
	case <-time.After(2 * time.Second):
		t.Fatal("download never started")
	}
	again, _ := e.call("start_download", args)
	if again["created"] != false || again["job"].(map[string]any)["job_id"] != id {
		t.Fatal(again)
	}
	out, isErr := e.call("remove_download", map[string]any{"job_id": id})
	if !isErr || errCode(out) != "job_active" {
		t.Fatal(out)
	}
	e.call("cancel_download", map[string]any{"job_id": id})
	job := e.waitState(id)
	if job["state"] != "canceled" || len(job["files"].([]any)) != 0 || job["error"].(map[string]any)["code"] != "canceled" {
		t.Fatal(job)
	}
}

func TestLimits(t *testing.T) {
	e := newEnv(t)
	e.start(map[string]any{"max_task_bytes": 30_000_000, "max_duration_seconds": 3600})
	url := "https://www.youtube.com/watch?v=abc123"
	expect := func(args map[string]any, code string) {
		t.Helper()
		out, isErr := e.call("start_download", args)
		if !isErr || errCode(out) != code {
			t.Fatalf("want %s: %v", code, out)
		}
	}
	expect(map[string]any{"url": url, "preset": "best"}, "too_large")
	e.engine.set(video(map[string]any{"duration": 7200.0}), nil)
	expect(map[string]any{"url": url}, "too_long")
	e.engine.set(video(map[string]any{"live_status": "is_live"}), nil)
	expect(map[string]any{"url": url}, "live_not_supported")
	e.engine.set(media.Info{"_type": "playlist", "entries": []any{map[string]any{}, map[string]any{}}}, nil)
	expect(map[string]any{"url": url}, "playlist_not_supported")
	e.engine.set(video(nil), nil)
	e.free.Store(gib + gib/100)
	expect(map[string]any{"url": url, "preset": "720p"}, "insufficient_space")
	expect(map[string]any{"url": url, "preset": "8k"}, "invalid_request")
	expect(map[string]any{"url": url, "subtitles": []any{"e n"}}, "invalid_request")
	expect(map[string]any{"url": url, "extra": 1}, "invalid_request")
	out, _ := e.call("start_download", map[string]any{"url": "https://secret.example/" + strings.Repeat("x", 3000)})
	if strings.Contains(out["error"].(map[string]any)["message"].(string), "secret.example") {
		t.Fatal("input echoed back")
	}
}

func TestAbortWhenDownloadExceedsLimit(t *testing.T) {
	e := newEnv(t)
	e.engine.info = video(map[string]any{"formats": []any{}}) // unknown size: only the runtime check applies
	e.engine.chunks = []int64{20_000_000, 20_000_000}
	e.start(map[string]any{"max_task_bytes": 30_000_000})
	out, _ := e.call("start_download", map[string]any{"url": "https://www.youtube.com/watch?v=abc123"})
	job := e.waitState(out["job"].(map[string]any)["job_id"].(string))
	if job["state"] != "failed" || job["error"].(map[string]any)["code"] != "too_large" {
		t.Fatal(job)
	}
}

func TestRestartMarksInterrupted(t *testing.T) {
	e := newEnv(t)
	e.engine.gate = make(chan struct{})
	e.start(nil)
	out, _ := e.call("start_download", map[string]any{"url": "https://www.youtube.com/watch?v=abc123"})
	id := out["job"].(map[string]any)["job_id"].(string)
	<-e.engine.started
	e.stop()
	close(e.engine.gate)
	e.engine.gate = nil
	e.start(nil)
	job, _ := e.call("get_download", map[string]any{"job_id": id})
	if job["state"] != "interrupted" {
		t.Fatal(job)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	e.start(map[string]any{"requests_per_minute": 2})
	for range 2 {
		if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, token); r.StatusCode != 200 {
			t.Fatal(r.Status)
		}
	}
	r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, token)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatal(r.Status)
	}
	if r := e.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, readToken); r.StatusCode != 200 {
		t.Fatal("budgets are per token")
	}
}
