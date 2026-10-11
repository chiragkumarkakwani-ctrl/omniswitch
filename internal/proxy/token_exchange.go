package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const accessTokenType = "urn:ietf:params:oauth:token-type:access_token"

type subjectTokenContextKey struct{}

// WithSubjectToken attaches a verified subject token for RFC 8693 delegation.
// Authentication middleware must call this after validating an external token;
// gateway API keys and caller-supplied identity headers are not subject tokens.
func WithSubjectToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, subjectTokenContextKey{}, token)
}

func extractSubjectTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(subjectTokenContextKey{}).(string)
	return token
}

type tokenCache struct {
	mu      sync.Mutex
	entries map[[32]byte]tokenCacheEntry
}

type tokenCacheEntry struct {
	accessToken string
	expiresAt   time.Time
}

func (c *tokenCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

func (h *Handler) getExchangeToken(ctx context.Context, target target) (string, error) {
	if target.tokenExchange == nil {
		return "", nil
	}
	subjectToken := extractSubjectTokenFromContext(ctx)
	if subjectToken == "" {
		return "", fmt.Errorf("token exchange requires an authenticated OIDC bearer token")
	}
	key := sha256.Sum256([]byte(subjectToken))
	cache := target.tokenCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry, ok := cache.entries[key]; ok && time.Now().Before(entry.expiresAt) {
		return entry.accessToken, nil
	}
	for key, entry := range cache.entries {
		if !time.Now().Before(entry.expiresAt) {
			delete(cache.entries, key)
		}
	}

	te := target.tokenExchange
	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token": {subjectToken}, "client_id": {te.ClientID},
	}
	for _, field := range []struct{ name, value, fallback string }{
		{"subject_token_type", te.SubjectTokenType, accessTokenType},
		{"requested_token_type", te.RequestedTokenType, accessTokenType},
		{"client_secret", te.ClientSecret, ""}, {"scope", te.Scope, ""},
		{"audience", te.Audience, ""}, {"resource", te.Resource, ""},
	} {
		value := field.value
		if value == "" {
			value = field.fallback
		}
		if value != "" {
			form.Set(field.name, value)
		}
	}
	if te.ActorToken != "" {
		form.Set("actor_token", te.ActorToken)
		actorType := te.ActorTokenType
		if actorType == "" {
			actorType = accessTokenType
		}
		form.Set("actor_token_type", actorType)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, te.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("invalid token exchange endpoint")
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.Header.Set("Accept", "application/json")
	client := *h.client
	// Redirects must not forward subject/client/actor credentials to another
	// endpoint, including an HTTP destination configured by a remote server.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(tokenReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("token exchange request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not echo authorization-server bodies or credential-bearing URLs.
		return "", fmt.Errorf("token exchange returned HTTP %d", resp.StatusCode)
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		IssuedToken string `json:"issued_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("invalid token exchange response")
	}
	if tokenResp.TokenType != "" && !strings.EqualFold(tokenResp.TokenType, "Bearer") {
		return "", fmt.Errorf("token exchange returned an unsupported token type")
	}
	accessToken := tokenResp.AccessToken
	if accessToken == "" {
		accessToken = tokenResp.IssuedToken
	}
	if accessToken == "" {
		return "", fmt.Errorf("token exchange response has no access token")
	}
	// An unspecified/non-positive lifetime is not safe to cache. Cache known
	// lifetimes with a 10% safety margin capped at one minute.
	if tokenResp.ExpiresIn > 0 {
		ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
		if te.TokenCacheTTL > 0 && te.TokenCacheTTL < ttl {
			ttl = te.TokenCacheTTL
		}
		margin := ttl / 10
		if margin > time.Minute {
			margin = time.Minute
		}
		if cache.entries == nil {
			cache.entries = make(map[[32]byte]tokenCacheEntry)
		}
		cache.entries[key] = tokenCacheEntry{accessToken: accessToken, expiresAt: time.Now().Add(ttl - margin)}
	}
	return accessToken, nil
}
