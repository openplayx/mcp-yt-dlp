package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openplayx/mcp-yt-dlp/src/internal/auth"
	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	cleared bool
}

func (w *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	w.cleared = t.IsZero()
	return nil
}

func TestFileDownload(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	out, failed := e.call("start_download", map[string]any{"url": "https://www.youtube.com/watch?v=abc123"})
	if failed {
		t.Fatal(out)
	}
	id := out["job"].(map[string]any)["job_id"].(string)
	job := e.waitState(id, "completed")
	files := job["files"].([]any)
	listed, _ := e.call("list_downloads", nil)
	listFiles := listed["jobs"].([]any)[0].(map[string]any)["files"].([]any)
	for index, item := range files {
		file := item.(map[string]any)
		path := file["download_path"].(string)
		if path != fmt.Sprintf("/files/%s/%d", id, index) || listFiles[index].(map[string]any)["download_path"] != path {
			t.Fatal("inconsistent download_path", file, listFiles)
		}
		name := filepath.Base(file["path"].(string))
		length := int64(file["bytes"].(float64))
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, _ := http.NewRequest(method, e.srv.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+readToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			wantBody := length
			if method == http.MethodHead {
				wantBody = 0
			}
			disposition := "attachment; filename*=UTF-8''" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
			if err != nil || resp.StatusCode != 200 || resp.ContentLength != length || int64(len(body)) != wantBody ||
				resp.Header.Get("Content-Disposition") != disposition || resp.Header.Get("ETag") == "" || resp.Header.Get("Accept-Ranges") != "bytes" {
				t.Fatalf("%s: status=%d headers=%v body=%d err=%v", method, resp.StatusCode, resp.Header, len(body), err)
			}
		}
		for _, test := range []struct {
			rangeValue string
			status     int
			bodyLength int
		}{
			{"bytes=1-1", 206, 1},
			{"bytes=-1", 206, 1},
			{"bytes=999999-", 416, -1},
		} {
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+readToken)
			req.Header.Set("Range", test.rangeValue)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != test.status || (test.bodyLength >= 0 && len(body) != test.bodyLength) || resp.Header.Get("Content-Range") == "" {
				t.Fatalf("range %s: %d %v %q", test.rangeValue, resp.StatusCode, resp.Header, body)
			}
		}
	}

	deadlineWriter := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	deadlineRequest := httptest.NewRequest(http.MethodHead, files[0].(map[string]any)["download_path"].(string), nil)
	deadlineRequest.Header.Set("Authorization", "Bearer "+readToken)
	e.srv.Config.Handler.ServeHTTP(deadlineWriter, deadlineRequest)
	if !deadlineWriter.cleared || deadlineWriter.Code != http.StatusOK {
		t.Fatal("authenticated file response retained the absolute write deadline")
	}

	// If-Range must serve a full representation when the validator changed;
	// otherwise clients could append bytes from two different versions.
	firstPath := files[0].(map[string]any)["download_path"].(string)
	head, _ := http.NewRequest(http.MethodHead, e.srv.URL+firstPath, nil)
	head.Header.Set("Authorization", "Bearer "+readToken)
	headResponse, err := http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	etag := headResponse.Header.Get("ETag")
	headResponse.Body.Close()
	for _, test := range []struct {
		validator string
		status    int
	}{
		{etag, http.StatusPartialContent},
		{`"stale"`, http.StatusOK},
	} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+firstPath, nil)
		req.Header.Set("Authorization", "Bearer "+readToken)
		req.Header.Set("Range", "bytes=1-")
		req.Header.Set("If-Range", test.validator)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("If-Range: got %d, want %d", response.StatusCode, test.status)
		}
	}

	path := files[0].(map[string]any)["download_path"].(string)
	for _, test := range []struct {
		path, token, origin string
		status              int
	}{
		{path, "", "", 401},
		{path, "invalid", "", 401},
		{path, readToken, "https://other.example", 403},
		{"/files/deadbeef/0", readToken, "", 404},
		{"/files/" + id + "/99", readToken, "", 404},
		{"/files/" + id + "/-1", readToken, "", 400},
		{"/files/" + id + "/invalid", readToken, "", 400},
	} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+test.path, nil)
		req.Header.Set("Authorization", "Bearer "+test.token)
		req.Header.Set("Origin", test.origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != test.status {
			t.Fatalf("%s: %d, want %d", test.path, resp.StatusCode, test.status)
		}
	}

	// Replacing a recorded file with a link outside DOWNLOAD_DIR must not
	// turn the file endpoint into an arbitrary filesystem reader.
	filePath := filepath.Join(e.dir, "dl", files[0].(map[string]any)["path"].(string))
	outside := filepath.Join(e.dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filePath); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+readToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("outside symlink served", resp.StatusCode)
	}
}

func TestFileRequiresReadScope(t *testing.T) {
	e := newEnv(t)
	e.start(nil)
	// Download permission alone does not grant HTTP file access, even for
	// an unknown job. Authentication and authorization run before lookup.
	secrets, err := config.ParseSecrets([]byte(`{"tokens":[{"name":"download-only","token":"` + token + `","scopes":["download"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Config.Handler.(*Server).auth = auth.New(secrets, 100)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/files/deadbeef/0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("download-only token accepted", resp.StatusCode)
	}
}
