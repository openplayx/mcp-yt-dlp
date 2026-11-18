package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
)

func TestParseLine(t *testing.T) {
	p, f := ParseLine(`[opx] {"status": "downloading", "downloaded_bytes": 1024, "total_bytes": null, "total_bytes_estimate": 4096.5, "filename": "/t/a.mp4", "speed": 12.5, "eta": 3}`)
	if p == nil || f != "" || p.Phase != "downloading" || *p.DownloadedBytes != 1024 || *p.TotalBytes != 4096 || p.Filename != "/t/a.mp4" || !p.TotalBytesEstimated {
		t.Fatalf("%+v", p)
	}
	p, _ = ParseLine(`[opx] {"status": "finished", "downloaded_bytes": 10, "total_bytes": 10, "filename": "x"}`)
	if p.Phase != "downloaded" || p.TotalBytesEstimated {
		t.Fatal(p.Phase)
	}
	p, _ = ParseLine("[opx-pp] Merger started")
	if p.Phase != "postprocessing" || p.Postprocessor != "Merger" {
		t.Fatalf("%+v", p)
	}
	if p, _ := ParseLine("[opx-pp] Merger finished"); p != nil {
		t.Fatal("finished postprocessor is not a progress report")
	}
	if _, f := ParseLine("[opx-file] /dl/Youtube/a [b].mp4"); f != "/dl/Youtube/a [b].mp4" {
		t.Fatal(f)
	}
	if p, f := ParseLine("[download] something"); p != nil || f != "" {
		t.Fatal("unrelated line")
	}
}

func TestProgressTotalSource(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   string
		total     int64
		estimated bool
	}{
		{"exact preferred", `{"total_bytes":100,"total_bytes_estimate":200}`, 100, false},
		{"zero exact uses estimate", `{"total_bytes":0,"total_bytes_estimate":200}`, 200, true},
		{"unknown", `{"total_bytes":null,"total_bytes_estimate":null}`, 0, false},
		{"zero estimate", `{"total_bytes_estimate":0}`, 0, false},
		{"finished corrects estimate", `{"status":"finished","downloaded_bytes":150,"total_bytes_estimate":200}`, 150, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := ParseLine("[opx] " + tc.payload)
			if p == nil {
				t.Fatal("missing progress")
			}
			if tc.total == 0 {
				if p.TotalBytes != nil {
					t.Fatalf("unknown total = %d", *p.TotalBytes)
				}
			} else if p.TotalBytes == nil || *p.TotalBytes != tc.total {
				t.Fatalf("total = %v, want %d", p.TotalBytes, tc.total)
			}
			if p.TotalBytesEstimated != tc.estimated {
				t.Fatalf("estimated = %v, want %v", p.TotalBytesEstimated, tc.estimated)
			}
		})
	}
}

func TestMapError(t *testing.T) {
	cases := map[string]string{
		"ERROR: No suitable extractor found for URL http://x":                        "unsupported_url",
		"ERROR: [generic] Unsupported URL: http://x":                                 "unsupported_url",
		"ERROR: [generic] x: Unable to download webpage: Connection to h timed out.": "upstream_timeout",
		"ERROR: [youtube] abc: Video unavailable":                                    "extraction_failed",
	}
	for msg, code := range cases {
		if e := MapError(msg); e.Code != code || strings.HasPrefix(e.Message, "ERROR") {
			t.Errorf("%s -> %+v", msg, e)
		}
	}
	if CleanMessage("\x1b[0;31mERROR:\x1b[0m boom", 400) != "boom" {
		t.Fatal(CleanMessage("\x1b[0;31mERROR:\x1b[0m boom", 400))
	}
}

func TestArgs(t *testing.T) {
	cfg := config.Default()
	limit := int64(2048)
	ref := "home"
	cfg.Network.RateLimitBytesPerSecond = &limit
	cfg.Network.AllowGenericExtractor = false
	cfg.Network.AllowedExtractors = []string{"Youtube"}
	cfg.Network.ProxyRef = &ref
	y := &YtDlp{bin: "yt-dlp", cfg: cfg, proxy: "socks5://p:1", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	probe := y.ProbeArgs("https://x/-o")
	if probe[len(probe)-2] != "--" || probe[len(probe)-1] != "https://x/-o" {
		t.Fatal("URL must follow --")
	}
	for _, want := range []string{"--ignore-config", "--use-extractors", "youtube,-generic", "--proxy", "--limit-rate", "--dump-single-json", "--flat-playlist", "--no-playlist"} {
		if !slices.Contains(probe, want) {
			t.Errorf("probe args lack %s: %v", want, probe)
		}
	}
	dl := y.DownloadArgs(DownloadSpec{URL: "https://x/v", Preset: "audio", Subtitles: []string{"en", "zh-Hans"}, Home: "/h", Temp: "/t"})
	for _, want := range []string{"--extract-audio", "m4a", "--write-subs", "en,zh-Hans", "srt", "home:/h", "temp:/t", "ba/b", "--max-filesize"} {
		if !slices.Contains(dl, want) {
			t.Errorf("download args lack %s: %v", want, dl)
		}
	}
}

func TestCollect(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Youtube")
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"A [x].mp4", "A [x].en.srt", "A [x].mkv", "A [x]2.mp4", "B.mp4"} {
		os.WriteFile(filepath.Join(dir, n), []byte("1"), 0o644)
	}
	outside := filepath.Join(t.TempDir(), "o.mp4")
	os.WriteFile(outside, []byte("1"), 0o644)
	got := Collect([]string{filepath.Join(dir, "A [x].mp4"), outside, filepath.Join(dir, "missing.mp4")}, home)
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	if !slices.Equal(names, []string{"A [x].en.srt", "A [x].mp4"}) {
		t.Fatal(names)
	}
}

// TestRealYtDlp runs the real executable against a local file server; set
// YTDLP_BIN to enable it (CI without yt-dlp skips it).
func TestRealYtDlp(t *testing.T) {
	bin := os.Getenv("YTDLP_BIN")
	if bin == "" {
		t.Skip("YTDLP_BIN not set")
	}
	payload := strings.Repeat("v", 300_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clip.mp4" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Now(), strings.NewReader(payload))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Storage.CacheDir = t.TempDir()
	y := NewYtDlp(bin, cfg, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if y.Version() == "unknown" {
		t.Fatal("no version")
	}
	ctx := context.Background()
	info, err := y.Probe(ctx, srv.URL+"/clip.mp4")
	if err != nil || info["id"] != "clip" {
		t.Fatal(info, err)
	}
	if _, err := y.Probe(ctx, srv.URL+"/missing"); err == nil {
		t.Fatal("expected an error")
	}
	home := t.TempDir()
	var reports int
	files, err := y.Download(ctx, DownloadSpec{URL: srv.URL + "/clip.mp4", Preset: "best", Home: home, Temp: filepath.Join(home, ".incomplete", "j")},
		func(p Progress) error { reports++; return nil })
	if err != nil || len(files) != 1 || reports == 0 {
		t.Fatal(files, err, reports)
	}
	if st, _ := os.Stat(files[0]); st.Size() != int64(len(payload)) {
		t.Fatal("size")
	}
	// An abort from the progress callback stops the run.
	_, err = y.Download(ctx, DownloadSpec{URL: srv.URL + "/clip.mp4?2", Preset: "best", Home: home, Temp: filepath.Join(home, ".incomplete", "k")},
		func(p Progress) error { return &Abort{"too_large", "stop"} })
	if a, ok := err.(*Abort); !ok || a.Code != "too_large" {
		t.Fatal(err)
	}
}
