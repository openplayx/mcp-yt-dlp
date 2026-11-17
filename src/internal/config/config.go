// Package config loads config.json and secrets.json.
//
// config.json holds no credentials; secrets.json holds API token digests
// and proxy URLs and is never committed. Paths, limits and network policy
// are an administrator decision; nothing here is reachable through the API.
// Both files are decoded strictly: unknown keys are an error.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
)

const GiB = int64(1) << 30

type Server struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	LogLevel string `json:"log_level"`
}

type Storage struct {
	Database string `json:"database"`
	// Finished files land here; partial files under <root>/.incomplete/<job>.
	DownloadRoot string `json:"download_root"`
	// yt-dlp cache (signature/player data).
	CacheDir string `json:"cache_dir"`
}

type Limits struct {
	MaxConcurrentDownloads int `json:"max_concurrent_downloads"`
	// Jobs waiting or running at once; further start_download calls fail.
	MaxActiveJobs int `json:"max_active_jobs"`
	// Per job, all streams and subtitles together.
	MaxTaskBytes int64 `json:"max_task_bytes"`
	// Never let downloads push free space on download_root below this.
	MinFreeBytes         int64   `json:"min_free_bytes"`
	MaxDurationSeconds   int64   `json:"max_duration_seconds"`
	InfoTimeoutSeconds   float64 `json:"info_timeout_seconds"`
	RequestsPerMinute    int     `json:"requests_per_minute"`
	MaxSubtitleLanguages int     `json:"max_subtitle_languages"`
}

type Network struct {
	// LAN test fixtures only; the default accepts public addresses only.
	AllowPrivateAddresses bool `json:"allow_private_addresses"`
	// Empty: every extractor yt-dlp has. Otherwise extractor keys, e.g.
	// ["Youtube", "Vimeo", "BiliBili"] (case-insensitive).
	AllowedExtractors []string `json:"allowed_extractors"`
	// The generic extractor fetches arbitrary pages; off makes the service
	// accept supported sites only.
	AllowGenericExtractor bool `json:"allow_generic_extractor"`
	// Key into secrets.json "proxies"; the URL may carry credentials.
	ProxyRef                *string `json:"proxy_ref"`
	SocketTimeoutSeconds    float64 `json:"socket_timeout_seconds"`
	RateLimitBytesPerSecond *int64  `json:"rate_limit_bytes_per_second"`
}

type Config struct {
	SchemaVersion int     `json:"schema_version"`
	Server        Server  `json:"server"`
	Storage       Storage `json:"storage"`
	Limits        Limits  `json:"limits"`
	Network       Network `json:"network"`
}

// Default is the configuration used when no file is given.
func Default() Config {
	return Config{
		SchemaVersion: 1,
		Server:        Server{Host: "127.0.0.1", Port: 8800, LogLevel: "INFO"},
		Storage: Storage{
			Database:     "data/yt-dlp.sqlite3",
			DownloadRoot: "data/downloads",
			CacheDir:     "data/cache",
		},
		Limits: Limits{
			MaxConcurrentDownloads: 1,
			MaxActiveJobs:          10,
			MaxTaskBytes:           4 * GiB,
			MinFreeBytes:           8 * GiB,
			MaxDurationSeconds:     4 * 3600,
			InfoTimeoutSeconds:     45,
			RequestsPerMinute:      120,
			MaxSubtitleLanguages:   5,
		},
		Network: Network{
			AllowedExtractors:     []string{},
			AllowGenericExtractor: true,
			SocketTimeoutSeconds:  20,
		},
	}
}

func (c *Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, a ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, a...))
		}
	}
	check(c.SchemaVersion == 1, "schema_version must be 1")
	check(c.Server.Port >= 1 && c.Server.Port <= 65535, "server.port must be 1-65535")
	check(slices.Contains([]string{"DEBUG", "INFO", "WARNING", "ERROR"}, c.Server.LogLevel),
		"server.log_level must be DEBUG, INFO, WARNING or ERROR")
	l := c.Limits
	check(l.MaxConcurrentDownloads >= 1 && l.MaxConcurrentDownloads <= 8, "limits.max_concurrent_downloads must be 1-8")
	check(l.MaxActiveJobs >= 1 && l.MaxActiveJobs <= 200, "limits.max_active_jobs must be 1-200")
	check(l.MaxTaskBytes >= 1<<20, "limits.max_task_bytes must be >= 1 MiB")
	check(l.MinFreeBytes >= 0, "limits.min_free_bytes must be >= 0")
	check(l.MaxDurationSeconds >= 1, "limits.max_duration_seconds must be >= 1")
	check(l.InfoTimeoutSeconds > 0 && l.InfoTimeoutSeconds <= 55, "limits.info_timeout_seconds must be in (0, 55]")
	check(l.RequestsPerMinute >= 1 && l.RequestsPerMinute <= 10000, "limits.requests_per_minute must be 1-10000")
	check(l.MaxSubtitleLanguages >= 0 && l.MaxSubtitleLanguages <= 20, "limits.max_subtitle_languages must be 0-20")
	n := c.Network
	check(n.SocketTimeoutSeconds > 0 && n.SocketTimeoutSeconds <= 120, "network.socket_timeout_seconds must be in (0, 120]")
	check(n.RateLimitBytesPerSecond == nil || *n.RateLimitBytesPerSecond >= 1024,
		"network.rate_limit_bytes_per_second must be >= 1024")
	return errors.Join(errs...)
}

type Token struct {
	Name string `json:"name"`
	// Either the plain token or its lowercase hex SHA-256.
	Token       *string  `json:"token"`
	TokenSHA256 *string  `json:"token_sha256"`
	Scopes      []string `json:"scopes"`
}

// Digest is the lowercase hex SHA-256 of the token.
func (t Token) Digest() string {
	if t.TokenSHA256 != nil {
		return *t.TokenSHA256
	}
	sum := sha256.Sum256([]byte(*t.Token))
	return hex.EncodeToString(sum[:])
}

type Secrets struct {
	Tokens  []Token           `json:"tokens"`
	Proxies map[string]string `json:"proxies"`
}

var (
	tokenName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	hex64     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	scopes    = []string{"read", "download"}
)

func (s *Secrets) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for i := range s.Tokens {
		t := &s.Tokens[i]
		if !tokenName.MatchString(t.Name) {
			errs = append(errs, fmt.Errorf("tokens[%d].name is invalid", i))
		}
		if seen[t.Name] {
			errs = append(errs, errors.New("token names must be unique"))
		}
		seen[t.Name] = true
		if (t.Token == nil) == (t.TokenSHA256 == nil) {
			errs = append(errs, fmt.Errorf("tokens[%d]: set exactly one of token / token_sha256", i))
		}
		if t.Token != nil && (len(*t.Token) < 24 || len(*t.Token) > 512) {
			errs = append(errs, fmt.Errorf("tokens[%d].token must be 24-512 characters", i))
		}
		if t.TokenSHA256 != nil && !hex64.MatchString(*t.TokenSHA256) {
			errs = append(errs, fmt.Errorf("tokens[%d].token_sha256 must be 64 lowercase hex characters", i))
		}
		if t.Scopes == nil {
			t.Scopes = []string{"read"}
		}
		for _, sc := range t.Scopes {
			if !slices.Contains(scopes, sc) {
				errs = append(errs, fmt.Errorf("tokens[%d]: unknown scope %q", i, sc))
			}
		}
	}
	if s.Proxies == nil {
		s.Proxies = map[string]string{}
	}
	return errors.Join(errs...)
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// Parse decodes config.json content over the defaults and validates it.
func Parse(data []byte) (Config, error) {
	c := Default()
	if err := decodeStrict(data, &c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	if c.Network.AllowedExtractors == nil {
		c.Network.AllowedExtractors = []string{}
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// Load reads config.json; an empty path gives the defaults.
func Load(path string) (Config, error) {
	if path == "" {
		return Default(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

func ParseSecrets(data []byte) (Secrets, error) {
	var s Secrets
	if err := decodeStrict(data, &s); err != nil {
		return s, fmt.Errorf("secrets: %w", err)
	}
	if err := s.Validate(); err != nil {
		return s, fmt.Errorf("secrets: %w", err)
	}
	return s, nil
}

// LoadSecrets reads secrets.json; an empty path gives no tokens.
func LoadSecrets(path string) (Secrets, error) {
	if path == "" {
		return Secrets{Proxies: map[string]string{}}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Secrets{}, err
	}
	return ParseSecrets(data)
}

// ResolveProxy returns the proxy URL named by network.proxy_ref, if any.
func ResolveProxy(c Config, s Secrets) (string, error) {
	if c.Network.ProxyRef == nil {
		return "", nil
	}
	p, ok := s.Proxies[*c.Network.ProxyRef]
	if !ok {
		return "", fmt.Errorf("proxy_ref %q is not defined in secrets", *c.Network.ProxyRef)
	}
	return p, nil
}
