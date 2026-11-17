// Package auth is bearer token authentication with per-token scopes and a
// per-token request budget (sliding one-minute window).
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openplayx/mcp-yt-dlp/src/internal/apierr"
	"github.com/openplayx/mcp-yt-dlp/src/internal/config"
)

type Principal struct {
	Name   string
	Scopes []string
}

func (p Principal) Has(scope string) bool { return slices.Contains(p.Scopes, scope) }

type entry struct {
	digest    []byte
	principal Principal
}

type Authenticator struct {
	tokens []entry
	rpm    int
	now    func() time.Time
	mu     sync.Mutex
	hits   map[string][]time.Time
}

func New(s config.Secrets, requestsPerMinute int) *Authenticator {
	a := &Authenticator{rpm: requestsPerMinute, now: time.Now, hits: map[string][]time.Time{}}
	for _, t := range s.Tokens {
		d, _ := hex.DecodeString(t.Digest())
		a.tokens = append(a.tokens, entry{d, Principal{t.Name, slices.Clone(t.Scopes)}})
	}
	return a
}

// SetClock replaces the clock (tests).
func (a *Authenticator) SetClock(now func() time.Time) { a.now = now }

func (a *Authenticator) Configured() bool { return len(a.tokens) > 0 }

// Authenticate checks an Authorization header value and takes one unit of
// the token's request budget.
func (a *Authenticator) Authenticate(header string) (Principal, *apierr.Error) {
	scheme, token, _ := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "bearer") || token == "" || len(token) > 512 {
		return Principal{}, apierr.Unauthorized()
	}
	sum := sha256.Sum256([]byte(token))
	var found *Principal
	for i := range a.tokens {
		// Compare every entry: timing does not reveal which one matched.
		if subtle.ConstantTimeCompare(a.tokens[i].digest, sum[:]) == 1 {
			found = &a.tokens[i].principal
		}
	}
	if found == nil {
		return Principal{}, apierr.Unauthorized()
	}
	if err := a.budget(found.Name); err != nil {
		return Principal{}, err
	}
	return *found, nil
}

func (a *Authenticator) budget(name string) *apierr.Error {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	hits := a.hits[name]
	cut := 0
	for cut < len(hits) && !hits[cut].After(now.Add(-time.Minute)) {
		cut++
	}
	hits = hits[cut:]
	if len(hits) >= a.rpm {
		a.hits[name] = hits
		wait := hits[0].Add(time.Minute).Sub(now).Seconds()
		return apierr.RateLimited(max(1, int(math.Ceil(wait))))
	}
	a.hits[name] = append(hits, now)
	return nil
}
