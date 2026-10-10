package limits

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryBackendAtomicWindows(t *testing.T) {
	now := time.Now()
	backend := NewMemoryBackendWithClock(func() time.Time { return now })
	ctx := context.Background()
	charges := []Charge{{Key: "policy:key", Amount: 6, Limit: 10, ExpiresAt: now.Add(time.Second)}, {Key: "other:key", Amount: 6, Limit: 5, ExpiresAt: now.Add(time.Second)}}
	used, denied, err := backend.Consume(ctx, charges)
	if err != nil || denied != 1 || used[0] != 0 || used[1] != 0 {
		t.Fatalf("partial atomic result: %v/%d/%v", used, denied, err)
	}
	charges[1].Limit = 10
	used, denied, err = backend.Consume(ctx, charges)
	if err != nil || denied != -1 || used[0] != 6 || used[1] != 6 {
		t.Fatalf("consume = %v/%d/%v", used, denied, err)
	}
	if err := backend.Adjust(ctx, []Charge{{Key: "policy:key", Amount: -2}, {Key: "other:key", Amount: -20}, {Key: "missing", Amount: 5}}); err != nil {
		t.Fatal(err)
	}
	counts, err := backend.Counts(ctx, []string{"policy:key", "other:key", "missing"})
	if err != nil || counts[0] != 4 || counts[1] != 0 || counts[2] != 0 {
		t.Fatalf("adjustment = %v/%v", counts, err)
	}
	list, err := backend.List(ctx, "policy:")
	if err != nil || len(list) != 1 || list["policy:key"] != 4 {
		t.Fatalf("list = %v/%v", list, err)
	}
	now = now.Add(time.Second)
	if err := backend.Adjust(ctx, []Charge{{Key: "policy:key", Amount: 100}}); err != nil {
		t.Fatal(err)
	}
	list, _ = backend.List(ctx, "")
	if len(list) != 0 {
		t.Fatal("expired counter was recreated")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := backend.Consume(ctx, charges); !errors.Is(err, context.Canceled) {
		t.Fatal("consume ignored cancellation")
	}
	if err := backend.Adjust(ctx, charges); !errors.Is(err, context.Canceled) {
		t.Fatal("adjust ignored cancellation")
	}
	if _, err := backend.Counts(ctx, []string{"key"}); !errors.Is(err, context.Canceled) {
		t.Fatal("counts ignored cancellation")
	}
	if _, err := backend.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("list ignored cancellation")
	}
}
