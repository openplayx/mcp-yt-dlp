package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileIsConfinedToCompletedOutputs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "视频.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, jobs: map[string]*Job{
		"complete": {State: "completed", Files: []File{{Path: "视频.mp4", Bytes: 5}}},
		"running":  {State: "running", Files: []File{{Path: "视频.mp4", Bytes: 5}}},
		"failed":   {State: "failed", Files: []File{{Path: "视频.mp4", Bytes: 5}}},
		"escape":   {State: "completed", Files: []File{{Path: "../outside"}}},
		"absolute": {State: "completed", Files: []File{{Path: filepath.Join(root, "视频.mp4")}}},
		"folder":   {State: "completed", Files: []File{{Path: "."}}},
		"missing":  {State: "completed", Files: []File{{Path: "missing.mp4"}}},
	}}
	f, err := m.OpenFile("complete", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, test := range []struct {
		id    string
		index int
		code  string
	}{
		{"unknown", 0, "not_found"},
		{"running", 0, "file_not_ready"},
		{"failed", 0, "file_not_ready"},
		{"complete", -1, "file_not_found"},
		{"complete", 1, "file_not_found"},
		{"escape", 0, "file_unavailable"},
		{"absolute", 0, "file_unavailable"},
		{"folder", 0, "file_unavailable"},
		{"missing", 0, "file_unavailable"},
	} {
		f, err := m.OpenFile(test.id, test.index)
		if f != nil {
			f.Close()
		}
		if err == nil || err.Code != test.code {
			t.Errorf("%s[%d]: %v, want %s", test.id, test.index, err, test.code)
		}
	}
}
