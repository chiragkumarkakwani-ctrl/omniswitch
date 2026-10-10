package limits

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options keeps the legacy requests/default-window policy enabled. Explicit
// policies are additional constraints, not a replacement for that quota.
type Options struct {
	Requests int
	Window   time.Duration
	Policies []Policy
	FailOpen bool
	Now      func() time.Time
}

type configuration struct {
	Options
}

type Enforcer struct {
	backend Backend
	config  atomic.Pointer[configuration]
}

func NewEnforcer(backend Backend, options Options) (*Enforcer, error) {
	if backend == nil {
		backend = NewMemoryBackend()
	}
	enforcer := &Enforcer{backend: backend}
	if err := enforcer.Update(options); err != nil {
		return nil, err
	}
	return enforcer, nil
}

// Update atomically hot-reloads policies while retaining counters. In-flight
// reservations reconcile against the policies and windows they reserved.
func (e *Enforcer) Update(options Options) error {
	if err := ValidatePolicies(options.Policies); err != nil {
		return err
	}
	if options.Requests <= 0 {
		options.Requests = 60
	}
	if options.Window <= 0 {
		options.Window = time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
		if previous := e.config.Load(); previous != nil {
			options.Now = previous.Now
		}
	}
	options.Policies = append([]Policy(nil), options.Policies...)
	for index := range options.Policies {
		if options.Policies[index].Action == "" {
			options.Policies[index].Action = Reject
		}
	}
	e.config.Store(&configuration{Options: options})
	return nil
}

type Snapshot struct {
	Name            string  `json:"name"`
	Scope           Scope   `json:"scope"`
	Unit            Unit    `json:"unit"`
	Limit           float64 `json:"limit"`
	Window          string  `json:"window"`
	Used            float64 `json:"used"`
	Remaining       float64 `json:"remaining"`
	ResetsInSeconds float64 `json:"resets_in_seconds"`
	Source          string  `json:"source"`
	Model           string  `json:"model,omitempty"`
	Action          Action  `json:"-"`
}

type Rejection struct {
	Policy     Policy
	RetryAfter time.Duration
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("policy %s: %g %s per %s exceeded", r.Policy.Name, r.Policy.Limit, r.Policy.Unit, r.Policy.Window)
}

// BackendError distinguishes an unavailable counter from a policy rejection.
type BackendError struct{ Err error }

func (e *BackendError) Error() string { return "rate limit backend unavailable" }
func (e *BackendError) Unwrap() error { return e.Err }

type selectedPolicy struct {
	Policy
	source string
	charge Charge
	model  string
}

func encode(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func policyPrefix(policy Policy, now time.Time) (string, time.Time) {
	duration, _ := ParseWindow(policy.Window)
	// Legacy default windows can be shorter than 1s or longer than 30d.
	if duration == 0 {
		duration, _ = time.ParseDuration(policy.Window)
	}
	start := now.UnixNano() / int64(duration) * int64(duration)
	prefix := strings.Join([]string{encode(policy.Name), string(policy.Scope), string(policy.Unit), strconv.FormatInt(int64(duration), 10), strconv.FormatInt(start, 10)}, ":") + ":"
	return prefix, time.Unix(0, start).Add(duration)
}

func scopeID(scope Scope, identity Identity, model string) string {
	switch scope {
	case ScopeKey:
		return identity.KeyID
	case ScopeWorkspace:
		return identity.WorkspaceID
	case ScopeOrganization:
		return identity.OrganizationID
	case ScopeModel:
		return model
	case ScopeGlobal:
		return "global"
	default:
		return ""
	}
}

func policiesFor(config *configuration, identity Identity) []selectedPolicy {
	requestLimit := config.Requests
	if identity.RateLimit > 0 {
		requestLimit = identity.RateLimit
	}
	source := "config"
	if identity.RateLimit > 0 {
		source = "key"
	}
	policies := []selectedPolicy{{Policy: Policy{Name: "default-requests", Scope: ScopeKey, Unit: Requests, Limit: float64(requestLimit), Window: config.Window.String(), Action: Reject}, source: source}}
	for _, implicit := range []Policy{
		{Name: "key-requests-per-minute", Scope: ScopeKey, Unit: Requests, Limit: float64(identity.RequestsPerMinute), Window: "1m", Action: Reject},
		{Name: "key-tokens-per-minute", Scope: ScopeKey, Unit: TotalTokens, Limit: float64(identity.TokensPerMinute), Window: "1m", Action: Reject},
		{Name: "key-usd-per-day", Scope: ScopeKey, Unit: USD, Limit: identity.USDPerDay, Window: "24h", Action: Reject},
	} {
		if implicit.Limit > 0 {
			policies = append(policies, selectedPolicy{Policy: implicit, source: "key"})
		}
	}
	for _, policy := range config.Policies {
		policies = append(policies, selectedPolicy{Policy: policy, source: "config"})
	}
	return policies
}

func selectPolicies(config *configuration, identity Identity, model string, requests bool) []selectedPolicy {
	var selected []selectedPolicy
	for _, policy := range policiesFor(config, identity) {
		id := scopeID(policy.Scope, identity, model)
		if (policy.Unit == Requests) != requests || id == "" || !policy.matches(identity, model, false) {
			continue
		}
		prefix, reset := policyPrefix(policy.Policy, config.Now())
		policy.charge = Charge{Key: prefix + encode(id), Limit: policy.scaledLimit(), ExpiresAt: reset}
		if policy.Action == Log {
			policy.charge.Limit = 0
		}
		policy.model = model
		selected = append(selected, policy)
	}
	return selected
}

// CheckRequests runs before routing and consumes only request-unit policies.
func (e *Enforcer) CheckRequests(ctx context.Context, identity Identity, model string) ([]Snapshot, error) {
	_, snapshots, err := e.consume(ctx, identity, model, Usage{}, true, false)
	return snapshots, err
}

// CheckModelRequests consumes only model-dependent request policies that could
// not be inspected in middleware (for example multipart uploads). Call once,
// after decoding the model, before invoking an upstream provider.
func (e *Enforcer) CheckModelRequests(ctx context.Context, identity Identity, model string) ([]Snapshot, error) {
	_, snapshots, err := e.consume(ctx, identity, model, Usage{}, true, true)
	return snapshots, err
}

// Reserve performs an approximate token/spend pre-check after model resolution.
// The caller must Commit actual usage, or Release if no upstream usage occurred.
func (e *Enforcer) Reserve(ctx context.Context, identity Identity, model string, estimate Usage) (*Reservation, []Snapshot, error) {
	return e.consume(ctx, identity, model, estimate, false, false)
}

func (e *Enforcer) consume(ctx context.Context, identity Identity, model string, usage Usage, requests, deferred bool) (*Reservation, []Snapshot, error) {
	config := e.config.Load()
	selected := selectPolicies(config, identity, model, requests)
	if deferred {
		filtered := selected[:0]
		for _, policy := range selected {
			if policy.Scope == ScopeModel || policy.Match.Model != "" {
				filtered = append(filtered, policy)
			}
		}
		selected = filtered
	}
	charges := make([]Charge, len(selected))
	for index := range selected {
		selected[index].charge.Amount = usage.amount(selected[index].Unit)
		charges[index] = selected[index].charge
	}
	used, denied, err := e.backend.Consume(ctx, charges)
	if err != nil {
		if config.FailOpen {
			log.Printf("rate limit pre-check failed open: %v", err)
			return nil, []Snapshot{}, nil
		}
		return nil, []Snapshot{}, &BackendError{Err: err}
	}
	snapshots, snapshotErr := e.Snapshot(ctx, identity, model)
	if denied >= 0 {
		policy := selected[denied]
		return nil, snapshots, &Rejection{Policy: policy.Policy, RetryAfter: max(time.Millisecond, policy.charge.ExpiresAt.Sub(config.Now()))}
	}
	if snapshotErr != nil {
		// Preserve the reservation so the caller can release it when a
		// fail-closed snapshot error prevents the upstream call.
		return &Reservation{enforcer: e, identity: identity, model: model, selected: selected}, snapshots, snapshotErr
	}
	for index, policy := range selected {
		if policy.Action == Log && used[index] > policy.scaledLimit() {
			log.Printf("rate limit policy %q exceeded (action=log, unit=%s)", policy.Name, policy.Unit)
		}
	}
	return &Reservation{enforcer: e, identity: identity, model: model, selected: selected}, snapshots, nil
}

// Reservation is concurrency-safe and commits/releases at most once.
type Reservation struct {
	mu        sync.Mutex
	enforcer  *Enforcer
	identity  Identity
	model     string
	selected  []selectedPolicy
	completed bool
}

func (r *Reservation) Commit(ctx context.Context, actual Usage) ([]Snapshot, error) {
	if r == nil {
		return []Snapshot{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.completed {
		charges := make([]Charge, len(r.selected))
		for index, policy := range r.selected {
			charges[index] = policy.charge
			charges[index].Amount = actual.amount(policy.Unit) - policy.charge.Amount
		}
		// A transport error may occur after Redis applied the adjustment. Never
		// apply a second delta for the same reservation.
		r.completed = true
		if err := r.enforcer.backend.Adjust(ctx, charges); err != nil {
			if !r.enforcer.config.Load().FailOpen {
				return nil, &BackendError{Err: err}
			}
			log.Printf("rate limit reconciliation failed open: %v", err)
		}
		// Never retry a potentially applied adjustment after a transport error.
		r.completed = true
	}
	return r.enforcer.Snapshot(ctx, r.identity, r.model)
}

func (r *Reservation) Release(ctx context.Context) error {
	if r == nil {
		return nil
	}
	_, err := r.Commit(ctx, Usage{})
	return err
}

func makeSnapshot(policy selectedPolicy, used int64, reset, now time.Time) Snapshot {
	return Snapshot{
		Name: policy.Name, Scope: policy.Scope, Unit: policy.Unit, Limit: policy.Limit, Window: policy.Window,
		Used: unscale(policy.Unit, used), Remaining: max(0, policy.Limit-unscale(policy.Unit, used)),
		ResetsInSeconds: max(0, reset.Sub(now).Seconds()), Source: policy.source, Action: policy.Action,
	}
}

// Snapshot reads current windows without consuming them. Without a model it
// includes observed model-scope buckets (with an additional model field).
func (e *Enforcer) Snapshot(ctx context.Context, identity Identity, model string) ([]Snapshot, error) {
	config := e.config.Load()
	now := config.Now()
	snapshots := make([]Snapshot, 0)
	for _, policy := range policiesFor(config, identity) {
		if !policy.matches(identity, model, true) {
			continue
		}
		prefix, reset := policyPrefix(policy.Policy, now)
		id := scopeID(policy.Scope, identity, model)
		if policy.Scope == ScopeModel && model == "" {
			counters, err := e.backend.List(ctx, prefix)
			if err != nil {
				return e.snapshotError(config, err)
			}
			var keys []string
			for key := range counters {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			found := false
			for _, key := range keys {
				decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, prefix))
				if err != nil || !matches(policy.Match.Model, string(decoded)) {
					continue
				}
				entry := makeSnapshot(policy, counters[key], reset, now)
				entry.Model = string(decoded)
				snapshots = append(snapshots, entry)
				found = true
			}
			if !found {
				snapshots = append(snapshots, makeSnapshot(policy, 0, reset, now))
			}
			continue
		}
		if id == "" {
			continue
		}
		used, err := e.backend.Counts(ctx, []string{prefix + encode(id)})
		if err != nil {
			return e.snapshotError(config, err)
		}
		entry := makeSnapshot(policy, used[0], reset, now)
		if policy.Scope == ScopeModel {
			entry.Model = model
		}
		snapshots = append(snapshots, entry)
	}
	return snapshots, nil
}

func (e *Enforcer) snapshotError(config *configuration, err error) ([]Snapshot, error) {
	if config.FailOpen {
		log.Printf("rate limit snapshot failed open: %v", err)
		return []Snapshot{}, nil
	}
	return nil, &BackendError{Err: err}
}

// Options returns a defensive copy of the active policy configuration.
func (e *Enforcer) Options() Options {
	options := e.config.Load().Options
	options.Policies = append([]Policy(nil), options.Policies...)
	return options
}
