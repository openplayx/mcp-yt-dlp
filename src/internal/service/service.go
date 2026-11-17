// Package service holds the business operations behind the MCP tools.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openplayx/mcp-yt-dlp/src/internal/apierr"
	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
	"github.com/openplayx/mcp-yt-dlp/src/internal/engine"
	"github.com/openplayx/mcp-yt-dlp/src/internal/jobs"
	"github.com/openplayx/mcp-yt-dlp/src/internal/media"
	"github.com/openplayx/mcp-yt-dlp/src/internal/netguard"
	"github.com/openplayx/mcp-yt-dlp/src/internal/ojson"
)

type Service struct {
	Config   config.Config
	Engine   engine.Engine
	Jobs     *jobs.Manager
	Version  string
	resolver netguard.Resolver
	probes   chan struct{}
}

func New(cfg config.Config, eng engine.Engine, jm *jobs.Manager, resolver netguard.Resolver, version string) *Service {
	return &Service{Config: cfg, Engine: eng, Jobs: jm, Version: version, resolver: resolver, probes: make(chan struct{}, 4)}
}

func (s *Service) check(ctx context.Context, url string) (string, *apierr.Error) {
	return netguard.CheckURL(ctx, url, s.Config.Network.AllowPrivateAddresses, s.resolver)
}

func (s *Service) probe(ctx context.Context, url string) (media.Info, *apierr.Error) {
	timeout := time.Duration(s.Config.Limits.InfoTimeoutSeconds * float64(time.Second))
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	case <-ctx.Done():
		return nil, apierr.Timeout()
	}
	info, err := s.Engine.Probe(ctx, url)
	if err == nil {
		return info, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return nil, apierr.Timeout()
	}
	if e, ok := engine.IsEngineError(err); ok {
		switch e.Code {
		case "upstream_timeout":
			return nil, apierr.Timeout()
		case "unsupported_url":
			return nil, apierr.UnsupportedURL(e.Message)
		}
		return nil, apierr.ExtractionFailed(e.Message)
	}
	return nil, apierr.ExtractionFailed(engine.CleanMessage(err.Error(), 400))
}

func (s *Service) Status() *ojson.Object {
	l := s.Config.Limits
	presets := []any{}
	for _, p := range media.Presets {
		presets = append(presets, ojson.New("preset", p.Key, "label", p.Label))
	}
	return ojson.New(
		"service", "openplayx-yt-dlp",
		"version", s.Version,
		"yt_dlp_version", s.Engine.Version(),
		"free_bytes", s.Jobs.FreeBytes(),
		"reserve_bytes", l.MinFreeBytes,
		"max_task_bytes", l.MaxTaskBytes,
		"max_duration_seconds", l.MaxDurationSeconds,
		"max_concurrent_downloads", l.MaxConcurrentDownloads,
		"jobs", s.Jobs.Counts(),
		"presets", presets,
		"generic_sites_allowed", s.Config.Network.AllowGenericExtractor,
	)
}

func (s *Service) Info(ctx context.Context, url string) (*ojson.Object, *apierr.Error) {
	url, aerr := s.check(ctx, url)
	if aerr != nil {
		return nil, aerr
	}
	info, aerr := s.probe(ctx, url)
	if aerr != nil {
		return nil, aerr
	}
	return media.Summarize(info, s.Config.Limits.MaxTaskBytes), nil
}

func strPtr(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func (s *Service) Start(ctx context.Context, principal, url, preset string, subtitles []string) (*ojson.Object, *apierr.Error) {
	l := s.Config.Limits
	url, aerr := s.check(ctx, url)
	if aerr != nil {
		return nil, aerr
	}
	if len(subtitles) > l.MaxSubtitleLanguages {
		return nil, apierr.InvalidRequest(fmt.Sprintf("At most %d subtitle languages.", l.MaxSubtitleLanguages))
	}
	if existing := s.Jobs.FindActive(url, preset); existing != nil {
		return ojson.New("created", false, "job", existing), nil
	}
	free := s.Jobs.FreeBytes()
	if free < l.MinFreeBytes {
		return nil, apierr.InsufficientSpace(free, l.MinFreeBytes)
	}
	info, aerr := s.probe(ctx, url)
	if aerr != nil {
		return nil, aerr
	}
	if kind, _ := media.Str(info, "_type"); kind == "playlist" || kind == "multi_video" {
		count, _ := media.Num(info, "playlist_count")
		if count == 0 {
			entries, _ := info["entries"].([]any)
			count = float64(len(entries))
		}
		return nil, apierr.PlaylistNotSupported(int(count))
	}
	live, _ := info["is_live"].(bool)
	status, _ := media.Str(info, "live_status")
	if live || status == "is_live" || status == "is_upcoming" {
		return nil, apierr.LiveNotSupported()
	}
	if d, ok := media.Num(info, "duration"); ok && d > float64(l.MaxDurationSeconds) {
		return nil, apierr.TooLong(int64(d), l.MaxDurationSeconds)
	}
	p, _ := media.Lookup(preset)
	est, hasEst := media.EstimateBytes(info, p)
	var estPtr *int64
	if hasEst {
		if est > l.MaxTaskBytes {
			return nil, apierr.TooLarge(est, l.MaxTaskBytes)
		}
		if free-est < l.MinFreeBytes {
			return nil, apierr.InsufficientSpace(free, est+l.MinFreeBytes)
		}
		estPtr = &est
	}
	target := url
	if w := strPtr(info["webpage_url"]); w != nil {
		target = *w
	}
	extractor := strPtr(info["extractor_key"])
	if extractor == nil {
		extractor = strPtr(info["extractor"])
	}
	job, aerr := s.Jobs.Submit(jobs.Submission{
		Principal: principal, URL: target, Preset: preset, Subtitles: subtitles,
		Title: strPtr(info["title"]), Extractor: extractor, EstimatedBytes: estPtr,
	})
	if aerr != nil {
		return nil, aerr
	}
	return ojson.New("created", true, "job", job), nil
}

func (s *Service) Get(id string) (*ojson.Object, *apierr.Error) { return s.Jobs.Get(id) }

func (s *Service) List(state string, limit int) *ojson.Object {
	return ojson.New("jobs", s.Jobs.List(state, limit))
}

func (s *Service) Cancel(id string) (*ojson.Object, *apierr.Error) { return s.Jobs.Cancel(id) }

func (s *Service) Remove(id string, deleteFiles bool) (*ojson.Object, *apierr.Error) {
	return s.Jobs.Remove(id, deleteFiles)
}
