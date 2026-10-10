package limits

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testEnforcer(t *testing.T, policies ...Policy) (*Enforcer, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	enforcer, err := NewEnforcer(NewMemoryBackendWithClock(clock), Options{Requests: 1000, Window: time.Minute, Policies: policies, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	return enforcer, &now
}

func findSnapshot(t *testing.T, snapshots []Snapshot, name string) Snapshot {
	t.Helper()
	for _, entry := range snapshots {
		if entry.Name == name {
			return entry
		}
	}
	t.Fatalf("snapshot %q missing in %+v", name, snapshots)
	return Snapshot{}
}

func TestTokenAcceptanceAndReconciliation(t *testing.T) {
	ctx := context.Background()
	enforcer, now := testEnforcer(t)
	identity := Identity{KeyID: "key", TokensPerMinute: 1000}
	reservation, snapshots, err := enforcer.Reserve(ctx, identity, "model", Usage{InputTokens: 500, OutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := findSnapshot(t, snapshots, "key-tokens-per-minute"); got.Used != 600 || got.Remaining != 400 || got.ResetsInSeconds != 60 || got.Source != "key" {
		t.Fatalf("snapshot = %+v", got)
	}
	if _, _, err := enforcer.Reserve(ctx, identity, "model", Usage{InputTokens: 500, OutputTokens: 100}); err == nil {
		t.Fatal("second reservation must fail")
	} else {
		var denial *Rejection
		if !errors.As(err, &denial) || denial.Policy.Name != "key-tokens-per-minute" || denial.RetryAfter != time.Minute {
			t.Fatalf("denial = %v", err)
		}
	}
	if _, err := reservation.Commit(ctx, Usage{InputTokens: 350, OutputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Commit(ctx, Usage{InputTokens: 900}); err != nil {
		t.Fatal(err)
	}
	snapshots, err = enforcer.Snapshot(ctx, identity, "model")
	if err != nil || findSnapshot(t, snapshots, "key-tokens-per-minute").Used != 400 {
		t.Fatalf("commit not idempotent: %+v/%v", snapshots, err)
	}
	other, _, err := enforcer.Reserve(ctx, identity, "model", Usage{InputTokens: 600})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := other.Commit(ctx, Usage{InputTokens: 750}); err != nil {
		t.Fatal(err)
	}
	snapshots, _ = enforcer.Snapshot(ctx, identity, "model")
	if findSnapshot(t, snapshots, "key-tokens-per-minute").Used != 0 {
		t.Fatal("late completion charged the next window")
	}
	if _, _, err := enforcer.Reserve(ctx, identity, "model", Usage{InputTokens: 600}); err != nil {
		t.Fatal("new window: ", err)
	}
}

func TestAllPoliciesAtomicAndFirstDenial(t *testing.T) {
	first := validPolicy()
	first.Name, first.Scope, first.Limit = "workspace", ScopeWorkspace, 1000
	second := first
	second.Name, second.Scope, second.Limit = "organization", ScopeOrganization, 500
	third := second
	third.Name, third.Scope, third.Limit = "global", ScopeGlobal, 1
	enforcer, _ := testEnforcer(t, first, second, third)
	identity := Identity{KeyID: "key", WorkspaceID: "ws", OrganizationID: "org"}
	_, snapshots, err := enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600})
	var denial *Rejection
	if !errors.As(err, &denial) || denial.Policy.Name != "organization" {
		t.Fatalf("first denial = %v", err)
	}
	for _, name := range []string{"workspace", "organization", "global"} {
		if findSnapshot(t, snapshots, name).Used != 0 {
			t.Fatal("partial reservation after rejection")
		}
	}
	third.Limit = 1000
	second.Limit = 1000
	if err := enforcer.Update(Options{Policies: []Policy{first, second, third}, Now: enforcer.Options().Now}); err != nil {
		t.Fatal(err)
	}
	reservation, _, err := enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600})
	if err != nil {
		t.Fatal(err)
	}
	identity.KeyID = "another"
	_, _, err = enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600})
	if !errors.As(err, &denial) || denial.Policy.Name != "workspace" {
		t.Fatalf("shared scope = %v", err)
	}
	if err := reservation.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots, _ = enforcer.Snapshot(context.Background(), identity, "model")
	if findSnapshot(t, snapshots, "workspace").Used != 0 {
		t.Fatal("release failed")
	}
}

func TestRequestsAndLiveKeyLimits(t *testing.T) {
	enforcer, now := testEnforcer(t)
	identity := Identity{KeyID: "key", RateLimit: 2, RequestsPerMinute: 1}
	if _, err := enforcer.CheckRequests(context.Background(), identity, ""); err != nil {
		t.Fatal(err)
	}
	_, err := enforcer.CheckRequests(context.Background(), identity, "")
	var denial *Rejection
	if !errors.As(err, &denial) || denial.Policy.Name != "key-requests-per-minute" {
		t.Fatalf("implicit RPM = %v", err)
	}
	identity.RequestsPerMinute = 3
	if _, err := enforcer.CheckRequests(context.Background(), identity, ""); err != nil {
		t.Fatal("live key PATCH did not apply: ", err)
	}
	identity.RateLimit = 1
	if _, err := enforcer.CheckRequests(context.Background(), identity, ""); !errors.As(err, &denial) || denial.Policy.Name != "default-requests" {
		t.Fatalf("legacy requests/default window = %v", err)
	}
	*now = now.Add(time.Minute)
	if _, err := enforcer.CheckRequests(context.Background(), identity, ""); err != nil {
		t.Fatal(err)
	}
}

func TestUSDLogAndActualOverEstimate(t *testing.T) {
	policy := validPolicy()
	policy.Name, policy.Unit, policy.Limit, policy.Action = "observe", InputTokens, 5, Log
	enforcer, _ := testEnforcer(t, policy)
	identity := Identity{KeyID: "key", USDPerDay: .001}
	reservation, snapshots, err := enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 10, USD: .0006})
	if err != nil || findSnapshot(t, snapshots, "observe").Used != 10 {
		t.Fatalf("log policy must not reject: %+v/%v", snapshots, err)
	}
	if _, _, err := enforcer.Reserve(context.Background(), identity, "model", Usage{USD: .0006}); err == nil {
		t.Fatal("USD limit did not reject")
	}
	if _, err := reservation.Commit(context.Background(), Usage{InputTokens: 100, USD: .0012}); err != nil {
		t.Fatal(err)
	}
	snapshots, _ = enforcer.Snapshot(context.Background(), identity, "model")
	if got := findSnapshot(t, snapshots, "key-usd-per-day"); got.Used != .0012 || got.Remaining != 0 {
		t.Fatalf("actual over estimate = %+v", got)
	}
	if _, _, err := enforcer.Reserve(context.Background(), identity, "model", Usage{USD: .000001}); err == nil {
		t.Fatal("overshoot must block next request")
	}
}

func TestGlobAndModelSnapshots(t *testing.T) {
	policy := validPolicy()
	policy.Name, policy.Scope, policy.Unit = "models", ScopeModel, Requests
	policy.Match = Match{KeyID: "key*", WorkspaceID: "dev", Model: "*/gpt-*"}
	enforcer, _ := testEnforcer(t, policy)
	identity := Identity{KeyID: "key-1", WorkspaceID: "dev"}
	if _, err := enforcer.CheckRequests(context.Background(), identity, "openai/gpt-mini"); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.CheckRequests(context.Background(), identity, "openai/gpt-full"); err != nil {
		t.Fatal(err)
	}
	snapshots, err := enforcer.Snapshot(context.Background(), identity, "")
	if err != nil {
		t.Fatal(err)
	}
	modelCount := 0
	for _, entry := range snapshots {
		if entry.Name == "models" {
			modelCount++
			if entry.Used != 1 || entry.Model == "" {
				t.Fatalf("model snapshot = %+v", entry)
			}
		}
	}
	if modelCount != 2 {
		t.Fatalf("observed models = %d", modelCount)
	}
	identity.KeyID = "other"
	snapshots, _ = enforcer.Snapshot(context.Background(), identity, "")
	if len(snapshots) != 1 {
		t.Fatalf("unmatched policy leaked: %+v", snapshots)
	}
	identity.KeyID = "key-2"
	if _, err := enforcer.CheckModelRequests(context.Background(), identity, "openai/gpt-mini"); err != nil {
		t.Fatal(err)
	}
	snapshots, _ = enforcer.Snapshot(context.Background(), identity, "openai/gpt-mini")
	if findSnapshot(t, snapshots, "models").Used != 2 || findSnapshot(t, snapshots, "default-requests").Used != 0 {
		t.Fatal("deferred request check must not double count the default quota")
	}
}

type faultBackend struct {
	Backend
	consumeErr, adjustErr, countsErr, listErr error
}

func (b *faultBackend) Consume(ctx context.Context, charges []Charge) ([]int64, int, error) {
	if b.consumeErr != nil {
		return nil, -1, b.consumeErr
	}
	return b.Backend.Consume(ctx, charges)
}
func (b *faultBackend) Adjust(ctx context.Context, charges []Charge) error {
	if b.adjustErr != nil {
		return b.adjustErr
	}
	return b.Backend.Adjust(ctx, charges)
}
func (b *faultBackend) Counts(ctx context.Context, keys []string) ([]int64, error) {
	if b.countsErr != nil {
		return nil, b.countsErr
	}
	return b.Backend.Counts(ctx, keys)
}
func (b *faultBackend) List(ctx context.Context, prefix string) (map[string]int64, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.Backend.List(ctx, prefix)
}

func TestBackendFailuresAndDefaults(t *testing.T) {
	identity := Identity{KeyID: "key", TokensPerMinute: 100}
	failure := errors.New("offline")
	for _, failOpen := range []bool{false, true} {
		backend := &faultBackend{Backend: NewMemoryBackend(), consumeErr: failure}
		enforcer, err := NewEnforcer(backend, Options{FailOpen: failOpen})
		if err != nil {
			t.Fatal(err)
		}
		_, err = enforcer.CheckRequests(context.Background(), identity, "model")
		if failOpen && err != nil {
			t.Fatal(err)
		}
		if !failOpen && !errors.Is(err, failure) {
			t.Fatalf("backend cause missing: %v", err)
		}
		reservation, _, err := enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 10})
		if failOpen && (err != nil || reservation != nil) {
			t.Fatalf("fail open = %v/%v", reservation, err)
		}
		backend.consumeErr = nil
		reservation, _, err = enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 10})
		if err != nil {
			t.Fatal(err)
		}
		backend.adjustErr = failure
		_, err = reservation.Commit(context.Background(), Usage{InputTokens: 5})
		if failOpen && err != nil {
			t.Fatal(err)
		}
		if !failOpen && !errors.Is(err, failure) {
			t.Fatalf("commit error = %v", err)
		}
		backend.adjustErr = nil
		snapshots, err := reservation.Commit(context.Background(), Usage{InputTokens: 5})
		if err != nil || findSnapshot(t, snapshots, "key-tokens-per-minute").Used != 10 {
			t.Fatal("an uncertain adjustment was retried")
		}
		backend.countsErr = failure
		_, err = enforcer.Snapshot(context.Background(), identity, "model")
		if failOpen && err != nil || !failOpen && !errors.Is(err, failure) {
			t.Fatalf("snapshot error = %v", err)
		}
	}
	enforcer, err := NewEnforcer(nil, Options{})
	if err != nil || enforcer.Options().Requests != 60 || enforcer.Options().Window != time.Minute {
		t.Fatalf("defaults = %+v/%v", enforcer.Options(), err)
	}
	policy := validPolicy()
	policy.Action = "invalid"
	if _, err := NewEnforcer(nil, Options{Policies: []Policy{policy}}); err == nil {
		t.Fatal("invalid construction succeeded")
	}
	var reservation *Reservation
	if _, err := reservation.Commit(context.Background(), Usage{}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (&BackendError{Err: failure}).Error() != "rate limit backend unavailable" {
		t.Fatal("backend details exposed")
	}
}

func TestUpdateKeepsUsageAndCopiesPolicies(t *testing.T) {
	policy := validPolicy()
	policy.Action = ""
	enforcer, _ := testEnforcer(t, policy)
	reservation, _, err := enforcer.Reserve(context.Background(), Identity{KeyID: "key"}, "model", Usage{InputTokens: 600})
	if err != nil {
		t.Fatal(err)
	}
	options := enforcer.Options()
	if options.Policies[0].Action != Reject {
		t.Fatal("default action missing")
	}
	options.Policies[0].Limit = 700
	if enforcer.Options().Policies[0].Limit != 1000 {
		t.Fatal("Options leaked a slice")
	}
	if err := enforcer.Update(options); err != nil {
		t.Fatal(err)
	}
	options.Policies[0].Limit = 1
	if enforcer.Options().Policies[0].Limit != 700 {
		t.Fatal("Update retained caller slice")
	}
	options.Policies[0].Window = "invalid"
	if err := enforcer.Update(options); err == nil {
		t.Fatal("invalid update succeeded")
	}
	if _, err := reservation.Commit(context.Background(), Usage{InputTokens: 650}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := enforcer.Reserve(context.Background(), Identity{KeyID: "key"}, "model", Usage{InputTokens: 100}); err == nil {
		t.Fatal("live limit did not retain old usage")
	}
}

func TestConcurrentReservationsDoNotOvershoot(t *testing.T) {
	enforcer, _ := testEnforcer(t)
	identity := Identity{KeyID: "key", TokensPerMinute: 1000}
	var successes atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 50; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			reservation, _, err := enforcer.Reserve(context.Background(), identity, "model", Usage{InputTokens: 100})
			if err == nil {
				successes.Add(1)
				_, _ = reservation.Commit(context.Background(), Usage{InputTokens: 100})
			}
		}()
	}
	group.Wait()
	if successes.Load() != 10 {
		t.Fatalf("successes = %d, want 10", successes.Load())
	}
}

func TestModelSnapshotBackendErrorAndMissingScopes(t *testing.T) {
	policy := validPolicy()
	policy.Name, policy.Scope = "models", ScopeModel
	backend := &faultBackend{Backend: NewMemoryBackend(), listErr: errors.New("scan unavailable")}
	enforcer, _ := NewEnforcer(backend, Options{Policies: []Policy{policy}})
	if _, err := enforcer.Snapshot(context.Background(), Identity{KeyID: "key"}, ""); err == nil {
		t.Fatal("model snapshot error lost")
	}
	backend.listErr = nil
	snapshots, err := enforcer.Snapshot(context.Background(), Identity{KeyID: "key"}, "")
	if err != nil || findSnapshot(t, snapshots, "models").Used != 0 {
		t.Fatalf("empty model snapshot = %+v/%v", snapshots, err)
	}
	policy.Name, policy.Scope = "organization", ScopeOrganization
	if err := enforcer.Update(Options{Policies: []Policy{policy}}); err != nil {
		t.Fatal(err)
	}
	snapshots, err = enforcer.Snapshot(context.Background(), Identity{}, "")
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("missing scope = %+v/%v", snapshots, err)
	}
}

func TestUpdateKeepsInjectedClockWhenOmitted(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	enforcer, err := NewEnforcer(nil, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := enforcer.Update(Options{Requests: 7, Window: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if !enforcer.Options().Now().Equal(now) {
		t.Fatal("reload replaced the injected clock")
	}
}
