// Package jobs holds download jobs: SQLite records, a bounded FIFO worker
// pool, cancel and limits.
//
// A job is created only after the URL passed the pre-checks in the service
// layer. Workers re-extract the URL (signed media URLs expire) and download
// into <root>/.incomplete/<job>; yt-dlp moves finished files to <root>.
// Jobs do not survive a restart: queued/running ones become "interrupted"
// and their partial files are removed.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/openplayx/mcp-yt-dlp/src/internal/apierr"
	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
	"github.com/openplayx/mcp-yt-dlp/src/internal/engine"
	"github.com/openplayx/mcp-yt-dlp/src/internal/ojson"
)

var (
	Active = []string{"queued", "running"}
	Final  = []string{"completed", "failed", "canceled", "interrupted"}
)

const Incomplete = ".incomplete"

func isActive(state string) bool { return slices.Contains(Active, state) }
func isFinal(state string) bool  { return slices.Contains(Final, state) }

type File struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Job struct {
	ID             string
	Principal      string
	URL            string
	Preset         string
	Subtitles      []string
	State          string
	Title          *string
	Extractor      *string
	EstimatedBytes *int64
	CreatedAt      float64
	StartedAt      *float64
	FinishedAt     *float64
	Progress       *ojson.Object
	Files          []File
	Error          *JobError

	cancel     context.CancelFunc
	ctx        context.Context
	cancelSet  bool
	cancelOnce sync.Once
}

func (j *Job) requestCancel() {
	j.cancelOnce.Do(func() {
		j.cancelSet = true
		if j.cancel != nil {
			j.cancel()
		}
	})
}

func iso(ts *float64) any {
	if ts == nil {
		return nil
	}
	return time.Unix(int64(*ts), 0).UTC().Format("2006-01-02T15:04:05Z")
}

func ptrOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// View is the job as clients see it. Call with the manager lock held.
func (j *Job) View() *ojson.Object {
	p := j.Progress.Clone()
	if isFinal(j.State) {
		p.Set("phase", j.State)
		p.Delete("speed_bps")
		p.Delete("eta_seconds")
		p.Delete("eta_estimated")
	}
	if j.State == "completed" {
		if done, ok := p.Get("overall_downloaded_bytes"); ok {
			p.Set("scope", "overall")
			p.Delete("stream_index")
			p.Set("downloaded_bytes", done)
			p.Set("total_bytes", done)
			p.Set("current_total_bytes", done)
			p.Set("total_bytes_estimated", false)
		}
		p.Set("percent", 100.0)
	}
	files := []any{}
	for index, file := range j.Files {
		files = append(files, ojson.New("path", file.Path, "bytes", file.Bytes,
			"download_path", fmt.Sprintf("/files/%s/%d", j.ID, index)))
	}
	subs := j.Subtitles
	if subs == nil {
		subs = []string{}
	}
	var jerr any
	if j.Error != nil {
		jerr = j.Error
	}
	return ojson.New(
		"job_id", j.ID,
		"state", j.State,
		"url", j.URL,
		"preset", j.Preset,
		"subtitles", subs,
		"title", ptrOrNil(j.Title),
		"extractor", ptrOrNil(j.Extractor),
		"estimated_bytes", ptrOrNil(j.EstimatedBytes),
		"created_at", iso(&j.CreatedAt),
		"started_at", iso(j.StartedAt),
		"finished_at", iso(j.FinishedAt),
		"progress", p,
		"files", files,
		"error", jerr,
	)
}

// ---- store -----------------------------------------------------------------

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, principal TEXT, url TEXT, preset TEXT, subtitles TEXT,
  state TEXT, title TEXT, extractor TEXT, estimated_bytes INTEGER,
  created_at REAL, started_at REAL, finished_at REAL,
  progress TEXT, files TEXT, error TEXT
);
CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created_at);
`

// Store persists jobs in SQLite (same schema as earlier releases).
type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	closed bool
}

func OpenStore(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Store) Save(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	var jerr any
	if j.Error != nil {
		jerr = j.Error
	}
	files := j.Files
	if files == nil {
		files = []File{}
	}
	subs := j.Subtitles
	if subs == nil {
		subs = []string{}
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO jobs (id, principal, url, preset, subtitles, state, title,
		extractor, estimated_bytes, created_at, started_at, finished_at, progress, files, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Principal, j.URL, j.Preset, jsonText(subs), j.State, j.Title, j.Extractor,
		j.EstimatedBytes, j.CreatedAt, j.StartedAt, j.FinishedAt, jsonText(j.Progress), jsonText(files), jsonText(jerr))
	if err != nil {
		slog.Error("job save failed", "id", j.ID, "err", err)
	}
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		_, _ = s.db.Exec("DELETE FROM jobs WHERE id = ?", id)
	}
}

func (s *Store) LoadAll() ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, principal, url, preset, subtitles, state, title, extractor,
		estimated_bytes, created_at, started_at, finished_at, progress, files, error FROM jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var (
			j                         Job
			principal, url, preset    sql.NullString
			subs, progress, files, je sql.NullString
			state                     sql.NullString
			title, extractor          sql.NullString
			est                       sql.NullInt64
			created                   sql.NullFloat64
			started, finished         sql.NullFloat64
		)
		if err := rows.Scan(&j.ID, &principal, &url, &preset, &subs, &state, &title, &extractor,
			&est, &created, &started, &finished, &progress, &files, &je); err != nil {
			return nil, err
		}
		j.Principal, j.URL, j.Preset, j.State = principal.String, url.String, preset.String, state.String
		j.CreatedAt = created.Float64
		if title.Valid {
			j.Title = &title.String
		}
		if extractor.Valid {
			j.Extractor = &extractor.String
		}
		if est.Valid {
			j.EstimatedBytes = &est.Int64
		}
		if started.Valid {
			j.StartedAt = &started.Float64
		}
		if finished.Valid {
			j.FinishedAt = &finished.Float64
		}
		_ = json.Unmarshal([]byte(subs.String), &j.Subtitles)
		j.Progress = &ojson.Object{}
		if progress.Valid {
			_ = json.Unmarshal([]byte(progress.String), j.Progress)
		}
		_ = json.Unmarshal([]byte(files.String), &j.Files)
		if je.Valid && je.String != "null" {
			_ = json.Unmarshal([]byte(je.String), &j.Error)
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.db.Close()
}

// ---- manager ---------------------------------------------------------------

// DiskFree returns the free bytes of the file system holding path.
type DiskFree func(path string) (int64, error)

type Manager struct {
	limits   config.Limits
	engine   engine.Engine
	store    *Store
	diskFree DiskFree
	now      func() float64
	log      *slog.Logger
	Root     string

	mu       sync.Mutex
	jobs     map[string]*Job
	queue    []*Job
	wake     *sync.Cond
	stopping bool
	running  int
	idle     *sync.Cond
	workers  sync.WaitGroup
}

func NewManager(cfg config.Config, eng engine.Engine, store *Store, diskFree DiskFree, log *slog.Logger) (*Manager, error) {
	root, err := filepath.Abs(cfg.Storage.DownloadRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	m := &Manager{
		limits: cfg.Limits, engine: eng, store: store, diskFree: diskFree, log: log, Root: root,
		now:  func() float64 { return float64(time.Now().UnixNano()) / 1e9 },
		jobs: map[string]*Job{},
	}
	m.wake = sync.NewCond(&m.mu)
	m.idle = sync.NewCond(&m.mu)
	if err := m.recover(); err != nil {
		return nil, err
	}
	for range cfg.Limits.MaxConcurrentDownloads {
		m.workers.Add(1)
		go m.worker()
	}
	return m, nil
}

func (m *Manager) recover() error {
	all, err := m.store.LoadAll()
	if err != nil {
		return err
	}
	for _, j := range all {
		if isActive(j.State) {
			j.State = "interrupted"
			if j.FinishedAt == nil {
				t := m.now()
				j.FinishedAt = &t
			}
			j.Error = &JobError{"interrupted", "The service restarted during this download."}
			m.store.Save(j)
		}
		m.jobs[j.ID] = j
	}
	return os.RemoveAll(filepath.Join(m.Root, Incomplete))
}

// Shutdown stops workers; running jobs end as "interrupted". It waits up to
// timeout for them to finish their bookkeeping.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	m.stopping = true
	for _, j := range m.jobs {
		j.requestCancel()
	}
	m.wake.Broadcast()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		for m.running > 0 {
			m.idle.Wait()
		}
		m.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (m *Manager) FreeBytes() int64 {
	n, err := m.diskFree(m.Root)
	if err != nil {
		m.log.Warn("disk usage failed", "err", err)
		return 0
	}
	return n
}

func (m *Manager) Get(id string) (*ojson.Object, *apierr.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, apierr.JobNotFound()
	}
	return j.View(), nil
}

// OpenFile opens only a completed job's recorded output. os.Root confines
// resolution (including symlinks) to the download root, even during renames.
// The returned descriptor remains valid if the job is subsequently removed.
func (m *Manager) OpenFile(id string, index int) (*os.File, *apierr.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, apierr.JobNotFound()
	}
	if j.State != "completed" {
		return nil, apierr.New("file_not_ready", http.StatusConflict, "Files are available only after the download completes.")
	}
	if index < 0 || index >= len(j.Files) {
		return nil, apierr.New("file_not_found", http.StatusNotFound, "Unknown file index.")
	}
	root, err := os.OpenRoot(m.Root)
	if err != nil {
		return nil, apierr.Internal()
	}
	defer root.Close()
	f, err := root.Open(j.Files[index].Path)
	if err != nil {
		return nil, apierr.New("file_unavailable", http.StatusNotFound, "The downloaded file is unavailable.")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, apierr.New("file_unavailable", http.StatusNotFound, "The downloaded file is unavailable.")
	}
	return f, nil
}

func (m *Manager) sorted() []*Job {
	list := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].CreatedAt > list[b].CreatedAt })
	return list
}

func (m *Manager) List(state string, limit int) []any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []any{}
	for _, j := range m.sorted() {
		if (state == "active" && !isActive(j.State)) || (state != "" && state != "active" && j.State != state) {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, j.View())
	}
	return out
}

func (m *Manager) Counts() *ojson.Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := &ojson.Object{}
	for _, s := range append(slices.Clone(Active), Final...) {
		o.Set(s, 0)
	}
	for _, j := range m.jobs {
		v, _ := o.Get(j.State)
		n, _ := v.(int)
		o.Set(j.State, n+1)
	}
	return o
}

// FindActive returns the view of a queued/running job for url and preset.
func (m *Manager) FindActive(url, preset string) *ojson.Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.sorted() {
		if isActive(j.State) && j.URL == url && j.Preset == preset {
			return j.View()
		}
	}
	return nil
}

type Submission struct {
	Principal      string
	URL            string
	Preset         string
	Subtitles      []string
	Title          *string
	Extractor      *string
	EstimatedBytes *int64
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) Submit(s Submission) (*ojson.Object, *apierr.Error) {
	m.mu.Lock()
	active := 0
	for _, j := range m.jobs {
		if isActive(j.State) {
			active++
		}
	}
	if active >= m.limits.MaxActiveJobs {
		m.mu.Unlock()
		return nil, apierr.QueueFull(m.limits.MaxActiveJobs)
	}
	j := &Job{
		ID: newID(), Principal: s.Principal, URL: s.URL, Preset: s.Preset, Subtitles: s.Subtitles,
		State: "queued", Title: s.Title, Extractor: s.Extractor, EstimatedBytes: s.EstimatedBytes,
		CreatedAt: m.now(), Progress: &ojson.Object{},
	}
	j.ctx, j.cancel = context.WithCancel(context.Background())
	m.jobs[j.ID] = j
	m.store.Save(j)
	m.queue = append(m.queue, j)
	m.wake.Signal()
	view := j.View()
	m.mu.Unlock()
	m.log.Info("job queued", "id", j.ID, "extractor", ptrOrNil(s.Extractor), "preset", s.Preset)
	return view, nil
}

func (m *Manager) Cancel(id string) (*ojson.Object, *apierr.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, apierr.JobNotFound()
	}
	if !isActive(j.State) {
		return j.View(), nil
	}
	j.requestCancel()
	if j.State == "queued" {
		m.finish(j, "canceled", nil)
	}
	return j.View(), nil
}

func (m *Manager) Remove(id string, deleteFiles bool) (*ojson.Object, *apierr.Error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, apierr.JobNotFound()
	}
	if isActive(j.State) {
		m.mu.Unlock()
		return nil, apierr.JobActive()
	}
	files := slices.Clone(j.Files)
	m.mu.Unlock()
	deleted := []string{}
	var freed int64
	if deleteFiles {
		for _, f := range files {
			path := filepath.Join(m.Root, f.Path)
			if real, err := filepath.EvalSymlinks(path); err == nil {
				path = real
			}
			st, err := os.Stat(path)
			if !engine.Under(m.Root, path) || err != nil || !st.Mode().IsRegular() {
				continue
			}
			if os.Remove(path) != nil {
				continue
			}
			freed += st.Size()
			deleted = append(deleted, f.Path)
			if parent := filepath.Dir(path); parent != m.Root {
				_ = os.Remove(parent) // only succeeds when empty
			}
		}
	}
	m.mu.Lock()
	delete(m.jobs, id)
	m.mu.Unlock()
	m.store.Delete(id)
	m.log.Info("job removed", "id", id, "files", len(deleted))
	kept := []string{}
	if !deleteFiles {
		for _, f := range files {
			kept = append(kept, f.Path)
		}
	}
	return ojson.New("job_id", id, "removed", true, "deleted_files", deleted,
		"freed_bytes", freed, "kept_files", kept), nil
}

// ---- worker ----------------------------------------------------------------

func (m *Manager) finish(j *Job, state string, jerr *JobError) {
	j.State = state
	j.Error = jerr
	t := m.now()
	j.FinishedAt = &t
	m.store.Save(j)
}

func (m *Manager) worker() {
	defer m.workers.Done()
	for {
		m.mu.Lock()
		for len(m.queue) == 0 && !m.stopping {
			m.wake.Wait()
		}
		if m.stopping {
			m.mu.Unlock()
			return
		}
		j := m.queue[0]
		m.queue = m.queue[1:]
		if j.State != "queued" || j.cancelSet {
			m.mu.Unlock()
			continue
		}
		j.State = "running"
		t := m.now()
		j.StartedAt = &t
		m.running++
		m.store.Save(j)
		m.mu.Unlock()

		m.download(j)

		m.mu.Lock()
		m.running--
		m.idle.Broadcast()
		m.mu.Unlock()
	}
}

func (m *Manager) download(j *Job) {
	temp := filepath.Join(m.Root, Incomplete, j.ID)
	spec := engine.DownloadSpec{URL: j.URL, Preset: j.Preset, Subtitles: j.Subtitles, Home: m.Root, Temp: temp}
	state, files := "failed", []File{}
	var jerr *JobError
	paths, err := m.safeDownload(j, spec)
	var abort *engine.Abort
	var eerr *engine.Error
	switch {
	case err == nil:
		for _, p := range paths {
			rel, rerr := filepath.Rel(m.Root, p)
			st, serr := os.Stat(p)
			if rerr == nil && serr == nil {
				files = append(files, File{Path: filepath.ToSlash(rel), Bytes: st.Size()})
			}
		}
		if len(files) > 0 {
			state = "completed"
		} else {
			jerr = &JobError{"no_output", "yt-dlp finished without producing a file (it may already exist or exceed max_filesize)."}
		}
	case errors.As(err, &abort):
		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		if abort.Code == "canceled" && stopping {
			state = "interrupted"
			jerr = &JobError{"interrupted", "The service stopped during this download."}
		} else {
			if abort.Code == "canceled" {
				state = "canceled"
			}
			jerr = &JobError{abort.Code, abort.Message}
		}
	case errors.As(err, &eerr):
		jerr = &JobError{eerr.Code, eerr.Message}
	default:
		m.log.Error("job crashed", "id", j.ID, "err", err)
		jerr = &JobError{"internal_error", "Unexpected server error."}
	}
	_ = os.RemoveAll(temp)
	m.mu.Lock()
	j.Files = files
	m.finish(j, state, jerr)
	m.mu.Unlock()
	code := "-"
	if jerr != nil {
		code = jerr.Code
	}
	m.log.Info("job finished", "id", j.ID, "state", state, "error", code, "files", len(files))
}

func (m *Manager) safeDownload(j *Job, spec engine.DownloadSpec) (paths []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return m.engine.Download(j.ctx, spec, m.progressFunc(j))
}

func (m *Manager) progressFunc(j *Job) engine.ProgressFunc {
	perFile := map[string]int64{}
	streamIndices := map[string]int{}
	var lastSave, lastDisk float64
	return func(d engine.Progress) error {
		m.mu.Lock()
		canceled := j.cancelSet
		m.mu.Unlock()
		if canceled {
			return &engine.Abort{Code: "canceled", Message: "Canceled by request."}
		}
		if d.DownloadedBytes != nil {
			perFile[d.Filename] = *d.DownloadedBytes
			if _, ok := streamIndices[d.Filename]; !ok {
				streamIndices[d.Filename] = len(streamIndices) + 1
			}
		}
		var total int64
		for _, n := range perFile {
			total += n
		}
		if total > m.limits.MaxTaskBytes {
			return &engine.Abort{Code: "too_large", Message: fmt.Sprintf(
				"Stopped after %d bytes: above the per-download limit of %d bytes.", total, m.limits.MaxTaskBytes)}
		}
		now := m.now()
		if now-lastDisk >= 2 {
			lastDisk = now
			if free := m.FreeBytes(); free < m.limits.MinFreeBytes {
				return &engine.Abort{Code: "insufficient_space", Message: fmt.Sprintf(
					"Stopped: free space fell to %d bytes, below the %d-byte reserve.", free, m.limits.MinFreeBytes)}
			}
		}
		// All displayed measurements describe the current stream. The aggregate
		// is kept separately for limits; later streams' sizes are not yet known.
		p := ojson.New("phase", d.Phase, "scope", "current_stream",
			"stream_index", streamIndices[d.Filename],
			"downloaded_bytes", ptrOrNil(d.DownloadedBytes),
			"overall_downloaded_bytes", total,
			"total_bytes", nil, "current_total_bytes", nil,
			"total_bytes_estimated", false, "percent", nil,
			"speed_bps", nil, "eta_seconds", nil, "eta_estimated", false)
		var speed int64
		if d.Phase == "downloading" && d.Speed != nil && *d.Speed > 0 {
			speed = int64(*d.Speed)
			if speed > 0 {
				p.Set("speed_bps", speed)
			}
		}
		if d.TotalBytes != nil && *d.TotalBytes > 0 && d.DownloadedBytes != nil &&
			*d.DownloadedBytes >= 0 && *d.DownloadedBytes <= *d.TotalBytes {
			p.Set("total_bytes", *d.TotalBytes)
			p.Set("current_total_bytes", *d.TotalBytes)
			p.Set("total_bytes_estimated", d.TotalBytesEstimated)
			p.Set("percent", math.Round(float64(*d.DownloadedBytes)*1000/float64(*d.TotalBytes))/10)
			if speed > 0 {
				p.Set("eta_seconds", math.Ceil(float64(*d.TotalBytes-*d.DownloadedBytes)/float64(speed)))
				p.Set("eta_estimated", true)
			}
		}
		if d.Phase == "postprocessing" {
			m.mu.Lock()
			p = j.Progress.Clone()
			m.mu.Unlock()
			p.Set("phase", d.Phase)
			p.Set("speed_bps", nil)
			p.Set("eta_seconds", nil)
			p.Set("eta_estimated", false)
		}
		if d.Postprocessor != "" {
			p.Set("postprocessor", d.Postprocessor)
		}
		m.mu.Lock()
		j.Progress = p
		save := now-lastSave >= 5
		if save {
			lastSave = now
			m.store.Save(j)
		}
		m.mu.Unlock()
		return nil
	}
}
