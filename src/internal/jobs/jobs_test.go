package jobs

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
	"github.com/openplayx/mcp-yt-dlp/src/internal/engine"
	"github.com/openplayx/mcp-yt-dlp/src/internal/ojson"
)

func progressTestManager() *Manager {
	return &Manager{limits: config.Default().Limits, now: func() float64 { return 0 }}
}

func progressView(t *testing.T, j *Job) map[string]any {
	t.Helper()
	// Exercise the JSON contract, including the persisted progress representation.
	data, err := json.Marshal(j.Progress)
	if err != nil {
		t.Fatal(err)
	}
	var restored ojson.Object
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	copy := &Job{State: j.State, EstimatedBytes: j.EstimatedBytes, Progress: &restored}
	view, _ := copy.View().Get("progress")
	data, err = json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func requireProgress(t *testing.T, j *Job, want map[string]any) {
	t.Helper()
	got := progressView(t, j)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v; progress: %v", k, got[k], v, got)
		}
	}
}

func TestMultiStreamProgress(t *testing.T) {
	m := progressTestManager()
	estimate := int64(1250) // Probe estimate must never become the denominator.
	j := &Job{State: "running", EstimatedBytes: &estimate}
	report := m.progressFunc(j)
	done, total, speed := int64(407), int64(671), float64(3.9)
	if err := report(engine.Progress{Phase: "downloading", Filename: "video", DownloadedBytes: &done,
		TotalBytes: &total, Speed: &speed, ETA: 79}); err != nil {
		t.Fatal(err)
	}
	requireProgress(t, j, map[string]any{"scope": "current_stream", "stream_index": float64(1),
		"downloaded_bytes": float64(407), "total_bytes": float64(671), "percent": 60.7,
		"speed_bps": float64(3), "eta_seconds": float64(88), "eta_estimated": true})
	done = 671
	if err := report(engine.Progress{Phase: "downloaded", Filename: "video", DownloadedBytes: &done, TotalBytes: &total}); err != nil {
		t.Fatal(err)
	}
	done, total, speed = 10, 100, 5
	if err := report(engine.Progress{Phase: "downloading", Filename: "audio", DownloadedBytes: &done,
		TotalBytes: &total, Speed: &speed, ETA: 99}); err != nil {
		t.Fatal(err)
	}
	requireProgress(t, j, map[string]any{"stream_index": float64(2), "downloaded_bytes": float64(10),
		"overall_downloaded_bytes": float64(681), "total_bytes": float64(100),
		"current_total_bytes": float64(100), "percent": float64(10), "eta_seconds": float64(18)})
	// Repeat reports must not double-count the stream.
	if err := report(engine.Progress{Phase: "downloaded", Filename: "audio", DownloadedBytes: &total, TotalBytes: &total}); err != nil {
		t.Fatal(err)
	}
	if err := report(engine.Progress{Phase: "postprocessing", Postprocessor: "Merger"}); err != nil {
		t.Fatal(err)
	}
	requireProgress(t, j, map[string]any{"phase": "postprocessing", "overall_downloaded_bytes": float64(771),
		"downloaded_bytes": float64(100), "percent": float64(100), "speed_bps": nil, "eta_seconds": nil})
	j.State = "completed"
	requireProgress(t, j, map[string]any{"scope": "overall", "stream_index": nil, "percent": float64(100),
		"downloaded_bytes": float64(771), "total_bytes": float64(771), "total_bytes_estimated": false})
}

func TestFragmentEstimatesAndUnknownTotal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  map[string]any
	}{
		{"changing estimate", []string{
			`[opx] {"status":"downloading","filename":"hls","downloaded_bytes":400,"total_bytes_estimate":828,"speed":4,"eta":79}`,
			`[opx] {"status":"downloading","filename":"hls","downloaded_bytes":407,"total_bytes_estimate":671,"speed":4,"eta":79}`,
		}, map[string]any{"downloaded_bytes": float64(407), "total_bytes": float64(671), "percent": 60.7,
			"eta_seconds": float64(66), "total_bytes_estimated": true}},
		{"unknown", []string{
			`[opx] {"status":"downloading","filename":"hls","downloaded_bytes":407,"speed":4,"eta":79}`,
		}, map[string]any{"downloaded_bytes": float64(407), "total_bytes": nil, "percent": nil, "eta_seconds": nil, "total_bytes_estimated": false}},
		{"outgrown estimate", []string{
			`[opx] {"status":"downloading","filename":"hls","downloaded_bytes":407,"total_bytes_estimate":400,"speed":4}`,
		}, map[string]any{"total_bytes": nil, "percent": nil, "eta_seconds": nil}},
		{"unknown becomes exact on finish", []string{
			`[opx] {"status":"finished","filename":"hls","downloaded_bytes":407,"total_bytes_estimate":671,"speed":4,"eta":79}`,
		}, map[string]any{"total_bytes": float64(407), "percent": float64(100), "total_bytes_estimated": false, "eta_seconds": nil, "speed_bps": nil}},
		{"missing speed", []string{
			`[opx] {"status":"downloading","filename":"hls","downloaded_bytes":0,"total_bytes":100,"eta":79}`,
		}, map[string]any{"percent": float64(0), "eta_seconds": nil, "speed_bps": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			estimate := int64(1000)
			j := &Job{State: "running", EstimatedBytes: &estimate}
			report := progressTestManager().progressFunc(j)
			for _, line := range tc.lines {
				p, _ := engine.ParseLine(line)
				if p == nil {
					t.Fatal("invalid fixture", line)
				}
				if err := report(*p); err != nil {
					t.Fatal(err)
				}
			}
			requireProgress(t, j, tc.want)
			j.State = "canceled"
			requireProgress(t, j, map[string]any{"phase": "canceled", "eta_seconds": nil, "speed_bps": nil})
		})
	}
}

func TestMultiStreamSizeLimit(t *testing.T) {
	m := progressTestManager()
	m.limits.MaxTaskBytes = 100
	report := m.progressFunc(&Job{})
	done := int64(60)
	for i, name := range []string{"video", "audio"} {
		err := report(engine.Progress{Phase: "downloading", Filename: name, DownloadedBytes: &done})
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			var abort *engine.Abort
			if !errors.As(err, &abort) || abort.Code != "too_large" {
				t.Fatalf("aggregate limit not enforced: %v", err)
			}
		}
	}
}
