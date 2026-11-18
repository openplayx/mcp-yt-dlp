// Package engine is the only code that runs yt-dlp.
//
// Engine is the seam tests replace. YtDlp runs the pinned yt-dlp executable
// once per call with options derived from config and the chosen preset
// only; no caller-supplied yt-dlp option ever reaches it, and the URL is
// passed after "--" so it can never be read as an option.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
	"github.com/openplayx/mcp-yt-dlp/src/internal/media"
)

// Outtmpl is the output template, relative to the download root.
const Outtmpl = "%(extractor_key)s/%(title).150B [%(id)s].%(ext)s"

// Error is a yt-dlp failure with a service error code (unsupported_url,
// upstream_timeout or extraction_failed).
type Error struct {
	Code, Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Abort stops a download (cancel or a limit); returned by progress callbacks.
type Abort struct {
	Code, Message string
}

func (a *Abort) Error() string { return a.Code + ": " + a.Message }

type DownloadSpec struct {
	URL       string
	Preset    string
	Subtitles []string
	Home      string
	Temp      string
}

// Progress is one progress report from a running download.
type Progress struct {
	Phase               string // downloading, downloaded, postprocessing
	Filename            string
	DownloadedBytes     *int64
	TotalBytes          *int64
	TotalBytesEstimated bool
	Speed               *float64
	ETA                 any
	Postprocessor       string
}

// ProgressFunc receives progress; a non-nil error (normally *Abort) stops
// the download.
type ProgressFunc func(Progress) error

type Engine interface {
	Version() string
	Probe(ctx context.Context, url string) (media.Info, error)
	// Download returns the absolute paths of the finished files (main
	// files plus sidecars such as subtitles).
	Download(ctx context.Context, spec DownloadSpec, progress ProgressFunc) ([]string, error)
}

var (
	ansi        = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	errorPrefix = regexp.MustCompile(`^(ERROR:\s*)+`)
)

// CleanMessage strips colours and the ERROR: prefix and caps the length.
func CleanMessage(text string, limit int) string {
	text = strings.TrimSpace(ansi.ReplaceAllString(text, ""))
	text = errorPrefix.ReplaceAllString(text, "")
	if r := []rune(text); len(r) > limit {
		text = string(r[:limit])
	}
	if text == "" {
		return "Unknown error."
	}
	return text
}

// MapError turns a yt-dlp error message into a service error code.
func MapError(msg string) *Error {
	msg = CleanMessage(msg, 400)
	switch {
	case strings.Contains(msg, "Unsupported URL") || strings.Contains(msg, "No suitable extractor"):
		return &Error{"unsupported_url", msg}
	case strings.Contains(strings.ToLower(msg), "timed out"):
		return &Error{"upstream_timeout", msg}
	}
	return &Error{"extraction_failed", msg}
}

// YtDlp runs the yt-dlp executable.
type YtDlp struct {
	bin     string
	cfg     config.Config
	proxy   string
	log     *slog.Logger
	version string
}

func NewYtDlp(bin string, cfg config.Config, proxy string, log *slog.Logger) *YtDlp {
	y := &YtDlp{bin: bin, cfg: cfg, proxy: proxy, log: log, version: "unknown"}
	if out, err := exec.Command(bin, "--version").Output(); err == nil {
		y.version = strings.TrimSpace(string(out))
	} else {
		log.Warn("yt-dlp --version failed", "bin", bin, "err", err)
	}
	return y
}

func (y *YtDlp) Version() string { return y.version }

func (y *YtDlp) baseArgs() []string {
	net := y.cfg.Network
	extractors := []string{}
	for _, e := range net.AllowedExtractors {
		extractors = append(extractors, strings.ToLower(e))
	}
	if len(extractors) == 0 {
		extractors = []string{"default"}
	}
	if !net.AllowGenericExtractor {
		extractors = append(extractors, "-generic")
	}
	args := []string{
		"--ignore-config", "--no-plugin-dirs", "--color", "no_color",
		"--socket-timeout", strconv.FormatFloat(net.SocketTimeoutSeconds, 'f', -1, 64),
		"--cache-dir", y.cfg.Storage.CacheDir,
		"--use-extractors", strings.Join(extractors, ","),
		"--no-playlist", "--windows-filenames", "--no-overwrites",
	}
	if y.proxy != "" {
		args = append(args, "--proxy", y.proxy)
	}
	if r := net.RateLimitBytesPerSecond; r != nil {
		args = append(args, "--limit-rate", strconv.FormatInt(*r, 10))
	}
	return args
}

// ProbeArgs are the arguments of a metadata-only run.
func (y *YtDlp) ProbeArgs(url string) []string {
	best, _ := media.Lookup("best")
	return append(y.baseArgs(), "--dump-single-json", "--flat-playlist",
		"-f", best.Format, "--ignore-no-formats-error", "--", url)
}

const (
	tagProgress = "[opx] "
	tagPost     = "[opx-pp] "
	tagFile     = "[opx-file] "
)

// DownloadArgs are the arguments of a download run.
func (y *YtDlp) DownloadArgs(spec DownloadSpec) []string {
	p, _ := media.Lookup(spec.Preset)
	args := append(y.baseArgs(),
		"--quiet", "--progress", "--newline",
		"--progress-template", "download:"+tagProgress+"%(progress)j",
		"--progress-template", "postprocess:"+tagPost+"%(progress.postprocessor)s %(progress.status)s",
		"--no-simulate", "--print", "after_move:"+tagFile+"%(filepath)s",
		"-f", p.Format, "--merge-output-format", "mp4/mkv",
		"-o", Outtmpl, "-P", "home:"+spec.Home, "-P", "temp:"+spec.Temp,
		// Belt and braces: playlists are rejected before queueing.
		"--playlist-items", "1",
		"--max-filesize", strconv.FormatInt(y.cfg.Limits.MaxTaskBytes, 10),
	)
	if p.AudioOnly {
		args = append(args, "--extract-audio", "--audio-format", "m4a")
	}
	if len(spec.Subtitles) > 0 {
		args = append(args, "--write-subs", "--sub-langs", strings.Join(spec.Subtitles, ","), "--convert-subs", "srt")
	}
	return append(args, "--", spec.URL)
}

func (y *YtDlp) command(args []string) *exec.Cmd {
	cmd := exec.Command(y.bin, args...)
	cmd.Env = os.Environ()
	setProcessGroup(cmd)
	return cmd
}

// run starts cmd, feeds every output line to onLine, and kills the process
// group when ctx ends or stop is closed.
func run(ctx context.Context, cmd *exec.Cmd, stdout io.Writer, onLine func(stream, line string)) error {
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var outPipe io.ReadCloser
	if stdout == nil {
		if outPipe, err = cmd.StdoutPipe(); err != nil {
			return err
		}
	} else {
		cmd.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killGroup(cmd)
		case <-done:
		}
	}()
	var wg sync.WaitGroup
	read := func(stream string, r io.Reader) {
		defer wg.Done()
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadString('\n')
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				onLine(stream, line)
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go read("stderr", errPipe)
	if outPipe != nil {
		wg.Add(1)
		go read("stdout", outPipe)
	}
	wg.Wait()
	err = cmd.Wait()
	close(done)
	return err
}

func (y *YtDlp) logLine(line string) {
	switch {
	case strings.HasPrefix(line, "WARNING:"):
		y.log.Info("yt-dlp warning", "detail", CleanMessage(line, 500))
	case strings.HasPrefix(line, "ERROR:"):
		y.log.Info("yt-dlp error", "detail", CleanMessage(line, 500))
	default:
		y.log.Debug("yt-dlp", "detail", CleanMessage(line, 1000))
	}
}

func (y *YtDlp) Probe(ctx context.Context, url string) (media.Info, error) {
	var out bytes.Buffer
	var lastErr string
	err := run(ctx, y.command(y.ProbeArgs(url)), &out, func(_, line string) {
		if strings.HasPrefix(line, "ERROR:") {
			lastErr = line
		}
		y.logLine(line)
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if lastErr == "" {
			lastErr = "yt-dlp failed: " + err.Error()
		}
		return nil, MapError(lastErr)
	}
	var info media.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info == nil {
		return nil, &Error{"extraction_failed", "yt-dlp returned no usable metadata."}
	}
	return info, nil
}

type progressJSON struct {
	Status             string   `json:"status"`
	Filename           string   `json:"filename"`
	DownloadedBytes    *float64 `json:"downloaded_bytes"`
	TotalBytes         *float64 `json:"total_bytes"`
	TotalBytesEstimate *float64 `json:"total_bytes_estimate"`
	Speed              *float64 `json:"speed"`
	ETA                any      `json:"eta"`
}

func toInt(f *float64) *int64 {
	if f == nil {
		return nil
	}
	n := int64(*f)
	return &n
}

// ParseLine turns one output line of a download run into a progress report
// or a finished file path.
func ParseLine(line string) (p *Progress, file string) {
	switch {
	case strings.HasPrefix(line, tagProgress):
		var j progressJSON
		if json.Unmarshal([]byte(line[len(tagProgress):]), &j) != nil {
			return nil, ""
		}
		phase := "downloading"
		if j.Status == "finished" {
			phase = "downloaded"
		}
		total := j.TotalBytes
		estimated := false
		if total == nil || *total <= 0 {
			total = j.TotalBytesEstimate
			estimated = total != nil && *total > 0
		}
		if phase == "downloaded" && j.DownloadedBytes != nil {
			total, estimated = j.DownloadedBytes, false
		}
		if total != nil && *total <= 0 {
			total, estimated = nil, false
		}
		return &Progress{Phase: phase, Filename: j.Filename, DownloadedBytes: toInt(j.DownloadedBytes),
			TotalBytes: toInt(total), TotalBytesEstimated: estimated, Speed: j.Speed, ETA: j.ETA}, ""
	case strings.HasPrefix(line, tagPost):
		name, status, _ := strings.Cut(line[len(tagPost):], " ")
		if status == "started" {
			return &Progress{Phase: "postprocessing", Postprocessor: name}, ""
		}
	case strings.HasPrefix(line, tagFile):
		return nil, line[len(tagFile):]
	}
	return nil, ""
}

func (y *YtDlp) Download(ctx context.Context, spec DownloadSpec, progress ProgressFunc) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu      sync.Mutex
		abort   error
		finals  []string
		lastErr string
	)
	err := run(ctx, y.command(y.DownloadArgs(spec)), nil, func(_, line string) {
		p, file := ParseLine(line)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case p != nil:
			if abort == nil {
				if err := progress(*p); err != nil {
					abort = err
					cancel()
				}
			}
		case file != "":
			finals = append(finals, file)
		default:
			if strings.HasPrefix(line, "ERROR:") {
				lastErr = line
			}
			y.logLine(line)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if abort != nil {
		return nil, abort
	}
	if ctx.Err() != nil {
		return nil, &Abort{"canceled", "Canceled by request."}
	}
	if err != nil {
		if lastErr == "" {
			lastErr = "yt-dlp failed: " + err.Error()
		}
		return nil, MapError(lastErr)
	}
	return Collect(finals, spec.Home), nil
}

// Collect returns the main files plus their sidecars (same stem plus a
// language tag, e.g. subtitles), keeping only files under home.
func Collect(finals []string, home string) []string {
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range finals {
		main, err := filepath.EvalSymlinks(raw)
		if err != nil {
			continue
		}
		if st, err := os.Stat(main); err != nil || !st.Mode().IsRegular() || !Under(home, main) {
			continue
		}
		dir, name := filepath.Split(main)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
				continue
			}
			if (n == name || isSidecar(n, stem)) && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// isSidecar matches "<stem>.<lang>.<ext>" (subtitles), not "<stem>.<ext>",
// which is the same video downloaded at another preset.
func isSidecar(name, stem string) bool {
	rest, ok := strings.CutPrefix(name, stem+".")
	return ok && strings.Contains(rest, ".")
}

// Under reports whether path is strictly inside dir.
func Under(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// IsEngineError unwraps an *Error.
func IsEngineError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
