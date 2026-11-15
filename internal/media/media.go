// Package media holds the quality presets and the compact media summary
// returned to clients.
//
// Clients never pass raw yt-dlp format strings or options; they pick one of
// the presets below. Size estimates are approximate (the site's declared
// sizes or bitrate x duration) and only used for limit checks and display.
package media

import (
	"fmt"
	"slices"
	"sort"

	"github.com/openplayx/mcp-yt-dlp/internal/ojson"
)

type Preset struct {
	Key       string
	Label     string
	Format    string
	MaxHeight int
	AudioOnly bool
}

// Best at or below the cap; if the site has nothing that small, the
// smallest available instead of failing.
func capped(h int) string {
	return fmt.Sprintf("bv*[height<=%d]+ba/b[height<=%d]/wv*+ba/w", h, h)
}

var Presets = []Preset{
	{Key: "best", Label: "Best video + audio", Format: "bv*+ba/b"},
	{Key: "2160p", Label: "Up to 2160p", Format: capped(2160), MaxHeight: 2160},
	{Key: "1080p", Label: "Up to 1080p", Format: capped(1080), MaxHeight: 1080},
	{Key: "720p", Label: "Up to 720p", Format: capped(720), MaxHeight: 720},
	{Key: "480p", Label: "Up to 480p", Format: capped(480), MaxHeight: 480},
	{Key: "audio", Label: "Audio only (m4a)", Format: "ba/b", AudioOnly: true},
}

const DefaultPreset = "1080p"

func PresetKeys() []string {
	out := make([]string, len(Presets))
	for i, p := range Presets {
		out[i] = p.Key
	}
	return out
}

func Lookup(key string) (Preset, bool) {
	i := slices.IndexFunc(Presets, func(p Preset) bool { return p.Key == key })
	if i < 0 {
		return Preset{}, false
	}
	return Presets[i], true
}

// Info is the JSON object yt-dlp prints for a URL (-J).
type Info = map[string]any

// Num returns a JSON number field; ok is false when it is absent, null,
// zero or not a number (Python truthiness, which the estimates rely on).
func Num(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, v != 0
	case int:
		return float64(v), v != 0
	case int64:
		return float64(v), v != 0
	}
	return 0, false
}

func numOr0(m map[string]any, key string) float64 {
	v, _ := Num(m, key)
	return v
}

// Str returns a non-empty string field.
func Str(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok && s != ""
}

// First returns the first of the keys holding a truthy value (Python `a or b`).
func First(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if truthy(m[k]) {
			return m[k]
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return m[keys[len(keys)-1]]
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func size(f map[string]any, duration float64, hasDuration bool) (int64, bool) {
	if s, ok := Num(f, "filesize"); ok {
		return int64(s), true
	}
	if s, ok := Num(f, "filesize_approx"); ok {
		return int64(s), true
	}
	if tbr, ok := Num(f, "tbr"); ok && hasDuration {
		return int64(tbr * 1000 / 8 * duration), true
	}
	return 0, false
}

func hasVideo(f map[string]any) bool {
	vc := f["vcodec"]
	if vc == nil {
		_, h := Num(f, "height")
		return h
	}
	s, isStr := vc.(string)
	return !isStr || s != "none"
}

func hasAudio(f map[string]any) bool {
	ac := f["acodec"]
	s, isStr := ac.(string)
	return ac != nil && (!isStr || s != "none")
}

func formats(info Info) []map[string]any {
	list, _ := info["formats"].([]any)
	out := []map[string]any{}
	for _, f := range list {
		if m, ok := f.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// maxBy returns the first element with the largest key (Python max()).
func maxBy(list []map[string]any, less func(a, b map[string]any) bool) map[string]any {
	var best map[string]any
	for _, f := range list {
		if best == nil || less(best, f) {
			best = f
		}
	}
	return best
}

// EstimateBytes estimates the download size of a preset; ok is false when
// the site gives nothing to go on.
func EstimateBytes(info Info, p Preset) (int64, bool) {
	fs := formats(info)
	duration, hasDuration := Num(info, "duration")
	if len(fs) == 0 {
		return size(info, duration, hasDuration)
	}
	var audio []map[string]any
	for _, f := range fs {
		if hasAudio(f) && !hasVideo(f) {
			audio = append(audio, f)
		}
	}
	abr := func(f map[string]any) float64 {
		if v, ok := Num(f, "abr"); ok {
			return v
		}
		return numOr0(f, "tbr")
	}
	bestAudio := maxBy(audio, func(a, b map[string]any) bool { return abr(b) > abr(a) })
	if p.AudioOnly {
		pick := bestAudio
		if pick == nil {
			pick = maxBy(fs, func(a, b map[string]any) bool { return numOr0(b, "tbr") > numOr0(a, "tbr") })
		}
		return size(pick, duration, hasDuration)
	}
	var video []map[string]any
	for _, f := range fs {
		if hasVideo(f) {
			video = append(video, f)
		}
	}
	if p.MaxHeight > 0 {
		var under []map[string]any
		for _, f := range video {
			if numOr0(f, "height") <= float64(p.MaxHeight) {
				under = append(under, f)
			}
		}
		if len(under) == 0 && len(video) > 0 {
			sorted := slices.Clone(video)
			sort.SliceStable(sorted, func(i, j int) bool { return numOr0(sorted[i], "height") < numOr0(sorted[j], "height") })
			under = sorted[:1]
		}
		video = under
	}
	if len(video) == 0 {
		return 0, false
	}
	pick := maxBy(video, func(a, b map[string]any) bool {
		ha, hb := numOr0(a, "height"), numOr0(b, "height")
		if ha != hb {
			return hb > ha
		}
		return numOr0(b, "tbr") > numOr0(a, "tbr")
	})
	n, ok := size(pick, duration, hasDuration)
	if !ok {
		return 0, false
	}
	if !hasAudio(pick) && bestAudio != nil {
		if extra, ok := size(bestAudio, duration, hasDuration); ok {
			n += extra
		}
	}
	return n, true
}

// Text caps a string field; nil when absent or empty.
func Text(v any, limit int) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

func optInt(n int64, ok bool) any {
	if !ok {
		return nil
	}
	return n
}

// Summarize is the get_media_info result.
func Summarize(info Info, maxTaskBytes int64) *ojson.Object {
	const maxEntries = 25
	kind, _ := Str(info, "_type")
	if kind == "" {
		kind = "video"
	}
	base := func(o *ojson.Object) *ojson.Object {
		return o.Set("id", info["id"]).
			Set("title", Text(info["title"], 300)).
			Set("extractor", First(info, "extractor_key", "extractor")).
			Set("webpage_url", First(info, "webpage_url", "original_url")).
			Set("uploader", Text(First(info, "uploader", "channel"), 200))
	}
	if kind == "playlist" || kind == "multi_video" {
		list, _ := info["entries"].([]any)
		var entries []map[string]any
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
		count := First(info, "playlist_count")
		if !truthy(count) {
			count = len(entries)
		}
		out := []any{}
		for i, e := range entries {
			if i >= maxEntries {
				break
			}
			out = append(out, ojson.New(
				"id", e["id"],
				"title", Text(e["title"], 300),
				"url", First(e, "webpage_url", "url"),
				"duration", e["duration"],
			))
		}
		o := base(ojson.New("kind", "playlist"))
		return o.Set("entry_count", count).
			Set("entries", out).
			Set("entries_truncated", len(entries) > maxEntries).
			Set("note", "Playlists are not downloaded as a whole; pass one entry URL to start_download.")
	}
	fs := formats(info)
	seen := map[int64]bool{}
	heights := []int64{}
	for _, f := range fs {
		if h, ok := Num(f, "height"); ok && hasVideo(f) && !seen[int64(h)] {
			seen[int64(h)] = true
			heights = append(heights, int64(h))
		}
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] > heights[j] })
	if len(heights) > 12 {
		heights = heights[:12]
	}
	presets := []any{}
	for _, p := range Presets {
		est, ok := EstimateBytes(info, p)
		var within any
		if ok {
			within = est <= maxTaskBytes
		}
		presets = append(presets, ojson.New(
			"preset", p.Key, "label", p.Label,
			"estimated_bytes", optInt(est, ok), "within_limit", within,
		))
	}
	subsMap, _ := info["subtitles"].(map[string]any)
	subs := make([]string, 0, len(subsMap))
	for k := range subsMap {
		subs = append(subs, k)
	}
	sort.Strings(subs)
	if len(subs) > 40 {
		subs = subs[:40]
	}
	var audio any
	if len(fs) > 0 {
		audio = slices.ContainsFunc(fs, hasAudio)
	}
	auto, _ := info["automatic_captions"].(map[string]any)
	o := base(ojson.New("kind", "video"))
	return o.Set("duration", info["duration"]).
		Set("upload_date", info["upload_date"]).
		Set("live_status", info["live_status"]).
		Set("thumbnail", info["thumbnail"]).
		Set("view_count", info["view_count"]).
		Set("description", Text(info["description"], 800)).
		Set("available_heights", heights).
		Set("has_audio", audio).
		Set("subtitle_languages", subs).
		Set("auto_caption_languages_count", len(auto)).
		Set("presets", presets)
}
