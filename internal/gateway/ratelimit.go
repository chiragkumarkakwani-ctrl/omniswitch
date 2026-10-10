package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/store"
)

type RateLimitBackend = limits.Backend

type RateLimiter struct {
	enforcer *limits.Enforcer
	store    *store.Store
}

func NewRateLimiter(limit int, interval time.Duration) *RateLimiter {
	return NewRateLimiterWithBackend(limit, interval, nil, false)
}

func NewRateLimiterWithBackend(limit int, interval time.Duration, backend RateLimitBackend, failOpen bool) *RateLimiter {
	enforcer, _ := limits.NewEnforcer(backend, limits.Options{Requests: limit, Window: interval, FailOpen: failOpen})
	return &RateLimiter{enforcer: enforcer}
}

func (rl *RateLimiter) Enforcer() *limits.Enforcer { return rl.enforcer }
func (rl *RateLimiter) SetStore(st *store.Store)   { rl.store = st }

// Headers runs outside authentication so even early /v1/* errors have the six
// OpenAI headers. Authenticated responses replace them with live counters.
func (rl *RateLimiter) Headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			limits.SetHeaders(w.Header(), nil)
		}
		next.ServeHTTP(w, r)
	})
}

type requestLimitCheckKey struct{}

type requestLimitCheck struct {
	modelKnown bool
	snapshots  []limits.Snapshot
}

func (rl *RateLimiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := inspectLimitModel(r)
		snapshots, err := rl.enforcer.CheckRequests(r.Context(), LimitIdentity(r), model)
		limits.SetHeaders(w.Header(), snapshots)
		if writeLimitFailure(w, r, rl.store, err) {
			return
		}
		ctx := context.WithValue(r.Context(), requestLimitCheckKey{}, requestLimitCheck{modelKnown: model != "", snapshots: snapshots})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LimitIdentity uses the key already loaded by authentication, never a second
// database lookup or untrusted quota headers on an authenticated request.
func LimitIdentity(r *http.Request) limits.Identity {
	if identity, ok := IdentityFromContext(r.Context()); ok {
		return limits.Identity{
			KeyID: identity.APIKeyID, WorkspaceID: identity.WorkspaceID, OrganizationID: identity.OrganizationID,
			RateLimit: identity.RateLimit, RequestsPerMinute: identity.RequestsPerMinute,
			TokensPerMinute: identity.TokensPerMinute, USDPerDay: identity.USDPerDay,
		}
	}
	limit, _ := strconv.Atoi(r.Header.Get("x-omniswitch-rate-limit"))
	keyID := firstNonEmpty(r.Header.Get("x-omniswitch-key-id"), "ip:"+remoteHost(r.RemoteAddr))
	return limits.Identity{KeyID: keyID, RateLimit: limit,
		WorkspaceID: r.Header.Get("x-omniswitch-workspace-id"), OrganizationID: r.Header.Get("x-omniswitch-organization-id")}
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

type restoredBody struct {
	io.Reader
	io.Closer
}

// Inspect only a bounded JSON prefix and restore both it and the unread tail.
// Multipart/large-prefix models are checked once when decoded by the handler.
func inspectLimitModel(r *http.Request) string {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/") || r.Body == nil {
		return ""
	}
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return ""
	}
	original := r.Body
	var consumed bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(io.LimitReader(original, 64<<10), &consumed))
	defer func() {
		r.Body = restoredBody{Reader: io.MultiReader(bytes.NewReader(consumed.Bytes()), original), Closer: original}
	}()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return ""
		}
		if field == "model" {
			var model string
			if decoder.Decode(&model) == nil {
				return model
			}
			return ""
		}
		var ignored json.RawMessage
		if decoder.Decode(&ignored) != nil {
			return ""
		}
	}
	return ""
}

// RejectLimit is the single rejection/audit call site. The integrator adds
// telemetry.DefaultMetrics.RecordRateLimitRejection(rejection.Policy.Name)
// here once P2d's registry is present. No payload is ever stored on this path.
func RejectLimit(w http.ResponseWriter, r *http.Request, st *store.Store, rejection *limits.Rejection) {
	w.Header().Set("Retry-After", limits.RetryAfter(rejection.RetryAfter))
	if st != nil {
		ctx := store.WithRequestEndpoint(r.Context(), r.URL.Path)
		keyID := r.Header.Get("x-omniswitch-key-id")
		if identity, ok := IdentityFromContext(ctx); ok {
			keyID = identity.APIKeyID
		}
		entry := store.RequestLog{
			ID: firstNonEmpty(limitContextID(ctx, "request_id"), newRequestID()), Timestamp: time.Now().UTC(),
			TraceID: limitContextID(ctx, "trace_id"), SessionID: limitContextID(ctx, "session_id"), APIKeyID: keyID,
			Endpoint: store.RequestEndpoint(ctx), CostSource: "none", Status: "limited", Decision: "DENY",
			DecisionReason: "rate_limit:" + rejection.Policy.Name,
		}
		entry.Model, _ = ctx.Value(resolvedLimitModelKey{}).(string)
		if entry.Model == "" {
			entry.Model = inspectLimitModel(r)
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := st.InsertLog(auditCtx, entry); err != nil {
			log.Printf("failed to log rate limit rejection: %v", err)
		}
	}
	if r.URL.Path == "/v1/messages" || store.RequestEndpoint(r.Context()) == "messages" {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"type": "error", "error": map[string]any{
			"type": "rate_limit_error", "message": rejection.Error(),
		}})
		return
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"type": "rate_limit_exceeded", "code": string(rejection.Policy.Unit) + "_limit_exceeded",
		"message": rejection.Error(), "param": nil,
	}})
}

type resolvedLimitModelKey struct{}

func writeLimitFailure(w http.ResponseWriter, r *http.Request, st *store.Store, err error) bool {
	if err == nil {
		return false
	}
	var rejection *limits.Rejection
	if errors.As(err, &rejection) {
		RejectLimit(w, r, st, rejection)
	} else {
		writeError(w, http.StatusServiceUnavailable, "rate limit backend unavailable")
	}
	return true
}

func limitContextID(ctx context.Context, name string) string {
	value, _ := ctx.Value(contextKey(name)).(string)
	return value
}
