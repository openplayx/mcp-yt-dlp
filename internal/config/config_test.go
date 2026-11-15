package config

import (
	"os"
	"strings"
	"testing"
)

func TestExamplesAreValid(t *testing.T) {
	data, err := os.ReadFile("../../deploy/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile("../../deploy/secrets.example.json")
	if _, err := ParseSecrets(data); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{
		`{"unknown": 1}`,
		`{"schema_version": 2}`,
		`{"limits": {"max_concurrent_downloads": 9}}`,
		`{"limits": {"info_timeout_seconds": 60}}`,
		`{"server": {"log_level": "TRACE"}}`,
		`{"network": {"rate_limit_bytes_per_second": 10}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, err := Parse([]byte(`{"limits": {"max_task_bytes": 2097152}}`))
	if err != nil || c.Limits.MaxTaskBytes != 2097152 || c.Limits.MaxActiveJobs != 10 {
		t.Fatal(c, err)
	}
	for _, bad := range []string{
		`{"tokens": [{"name": "a", "scopes": ["read"]}]}`,
		`{"tokens": [{"name": "a", "token": "short"}]}`,
		`{"tokens": [{"name": "a", "token": "` + strings.Repeat("x", 30) + `", "scopes": ["admin"]}]}`,
		`{"tokens": [{"name": "a", "token": "` + strings.Repeat("x", 30) + `"}, {"name": "a", "token": "` + strings.Repeat("y", 30) + `"}]}`,
	} {
		if _, err := ParseSecrets([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	s, err := ParseSecrets([]byte(`{"tokens": [{"name": "a", "token": "` + strings.Repeat("x", 30) + `"}]}`))
	if err != nil || s.Tokens[0].Scopes[0] != "read" || len(s.Tokens[0].Digest()) != 64 {
		t.Fatal(s, err)
	}
	cfg, _ := Parse([]byte(`{"network": {"proxy_ref": "home"}}`))
	if _, err := ResolveProxy(cfg, Secrets{}); err == nil {
		t.Fatal("undefined proxy_ref")
	}
	if p, _ := ResolveProxy(cfg, Secrets{Proxies: map[string]string{"home": "socks5://h:1"}}); p != "socks5://h:1" {
		t.Fatal(p)
	}
}
