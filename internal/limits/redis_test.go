package limits

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func redisBackendForTest(t *testing.T) *RedisBackend {
	t.Helper()
	url := os.Getenv("OMNISWITCH_TEST_REDIS_URL")
	if url == "" {
		t.Skip("OMNISWITCH_TEST_REDIS_URL is not set")
	}
	backend, err := NewRedisBackend(url, fmt.Sprintf("limits-test:%s:%d:", t.Name(), time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestRedisSharesOneWindowAcrossEnforcers(t *testing.T) {
	backend := redisBackendForTest(t)
	// Freeze counter identity and keep expiry safely in the future even when
	// this test starts just before a wall-clock minute boundary.
	now := time.Now().Add(5 * time.Second)
	options := Options{Requests: 1000, Now: func() time.Time { return now }}
	first, err := NewEnforcer(backend, options)
	if err != nil {
		t.Fatal(err)
	}
	otherBackend, err := NewRedisBackend(os.Getenv("OMNISWITCH_TEST_REDIS_URL"), backend.prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer otherBackend.Close()
	second, _ := NewEnforcer(otherBackend, options)
	identity := Identity{KeyID: "key", TokensPerMinute: 1000, USDPerDay: 1}
	reservation, _, err := first.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600, USD: .6})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600, USD: .6}); err == nil {
		t.Fatal("second replica bypassed shared window")
	}
	snapshots, err := second.Snapshot(context.Background(), identity, "model")
	if err != nil || findSnapshot(t, snapshots, "key-tokens-per-minute").Remaining != 400 {
		t.Fatalf("shared snapshot = %+v/%v", snapshots, err)
	}
	if _, err := reservation.Commit(context.Background(), Usage{InputTokens: 300, USD: .3}); err != nil {
		t.Fatal(err)
	}
	other, _, err := second.Reserve(context.Background(), identity, "model", Usage{InputTokens: 600, USD: .6})
	if err != nil {
		t.Fatal("shared reconciliation was not visible: ", err)
	}
	if err := other.Release(context.Background()); err != nil {
		t.Fatal(err)
	}

	var successes atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			current := first
			if index%2 == 1 {
				current = second
			}
			if _, _, err := current.Reserve(context.Background(), identity, "model", Usage{InputTokens: 100}); err == nil {
				successes.Add(1)
			}
		}(index)
	}
	group.Wait()
	if successes.Load() != 7 {
		t.Fatalf("concurrent replicas accepted %d, want 7", successes.Load())
	}
}

func TestRedisAtomicAdjustExpiryAndModelSnapshot(t *testing.T) {
	backend := redisBackendForTest(t)
	ctx := context.Background()
	expires := time.Now().Add(200 * time.Millisecond)
	charges := []Charge{{Key: "expiry:first", Amount: 6, Limit: 10, ExpiresAt: expires}, {Key: "expiry:second", Amount: 6, Limit: 5, ExpiresAt: expires}}
	used, denied, err := backend.Consume(ctx, charges)
	if err != nil || denied != 1 || used[0] != 0 {
		t.Fatalf("atomic refusal = %v/%d/%v", used, denied, err)
	}
	charges[1].Limit = 10
	if _, denied, err := backend.Consume(ctx, charges); err != nil || denied != -1 {
		t.Fatalf("consume = %d/%v", denied, err)
	}
	if err := backend.Adjust(ctx, []Charge{{Key: "expiry:first", Amount: -2}, {Key: "expiry:second", Amount: -100}, {Key: "missing", Amount: 10}}); err != nil {
		t.Fatal(err)
	}
	used, err = backend.Counts(ctx, []string{"expiry:first", "expiry:second", "missing"})
	if err != nil || used[0] != 4 || used[1] != 0 || used[2] != 0 {
		t.Fatalf("adjust = %v/%v", used, err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := backend.Adjust(ctx, []Charge{{Key: "expiry:first", Amount: 100}}); err != nil {
		t.Fatal(err)
	}
	used, _ = backend.Counts(ctx, []string{"expiry:first"})
	if used[0] != 0 {
		t.Fatal("expired reservation recreated a counter")
	}
	if _, _, err := backend.Consume(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := backend.Adjust(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if values, err := backend.Counts(ctx, nil); err != nil || len(values) != 0 {
		t.Fatal("empty counts")
	}
	policy := validPolicy()
	policy.Name, policy.Scope = "models", ScopeModel
	enforcer, _ := NewEnforcer(backend, Options{Policies: []Policy{policy}})
	if _, _, err := enforcer.Reserve(ctx, Identity{KeyID: "key"}, "openai/gpt-mini", Usage{InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := enforcer.Snapshot(ctx, Identity{KeyID: "key"}, "")
	if err != nil || findSnapshot(t, snapshots, "models").Used != 10 {
		t.Fatalf("model snapshot = %+v/%v", snapshots, err)
	}
	if err := backend.client.Set(ctx, backend.prefix+"invalid", "not-a-number", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Counts(ctx, []string{"invalid"}); err == nil {
		t.Fatal("invalid counter accepted")
	}
	if _, err := backend.List(ctx, "invalid"); err == nil {
		t.Fatal("invalid scan counter accepted")
	}
	if _, _, err := backend.Consume(ctx, []Charge{{Key: "invalid", Amount: 1, Limit: 1, ExpiresAt: time.Now().Add(time.Minute)}}); err == nil {
		t.Fatal("Lua error was hidden")
	}
}

func TestRedisConstructorAndClosedBackend(t *testing.T) {
	if _, err := NewRedisBackend("://bad", ""); err == nil {
		t.Fatal("invalid URL accepted")
	}
	backend, err := NewRedisBackend("rediss://localhost:6379/0", "")
	if err != nil || backend.prefix != "omniswitch:ratelimit:" {
		t.Fatalf("default prefix = %v/%v", backend, err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := backend.Ping(ctx); err == nil {
		t.Fatal("ping on closed backend")
	}
	if _, _, err := backend.Consume(ctx, []Charge{{Key: "key", Amount: 1, Limit: 2, ExpiresAt: time.Now().Add(time.Minute)}}); err == nil {
		t.Fatal("consume on closed backend")
	}
	if err := backend.Adjust(ctx, []Charge{{Key: "key", Amount: -1}}); err == nil {
		t.Fatal("adjust on closed backend")
	}
	if _, err := backend.Counts(ctx, []string{"key"}); err == nil {
		t.Fatal("count on closed backend")
	}
	if _, err := backend.List(ctx, ""); err == nil {
		t.Fatal("list on closed backend")
	}
}
