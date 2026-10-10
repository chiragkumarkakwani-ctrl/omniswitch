package limits

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Charge binds a counter to a particular window. Reconciliation never moves a
// late completion into the next window. A zero Limit records without rejecting.
type Charge struct {
	Key       string
	Amount    int64
	Limit     int64
	ExpiresAt time.Time
}

// Backend atomically checks and consumes all charges or none. Denied is the
// first failed charge's index, or -1. Adjust only changes existing counters.
type Backend interface {
	Consume(context.Context, []Charge) (used []int64, denied int, err error)
	Adjust(context.Context, []Charge) error
	Counts(context.Context, []string) ([]int64, error)
	List(context.Context, string) (map[string]int64, error)
}

type memoryCounter struct {
	used      int64
	expiresAt time.Time
}

// MemoryBackend is safe for concurrent use within one gateway process.
type MemoryBackend struct {
	mu       sync.Mutex
	counters map[string]memoryCounter
	now      func() time.Time
}

func NewMemoryBackend() *MemoryBackend { return NewMemoryBackendWithClock(time.Now) }

// NewMemoryBackendWithClock allows deterministic window-expiry tests.
func NewMemoryBackendWithClock(now func() time.Time) *MemoryBackend {
	return &MemoryBackend{counters: make(map[string]memoryCounter), now: now}
}

func (b *MemoryBackend) prune() {
	now := b.now()
	for key, counter := range b.counters {
		if !now.Before(counter.expiresAt) {
			delete(b.counters, key)
		}
	}
}

func (b *MemoryBackend) Consume(ctx context.Context, charges []Charge) ([]int64, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, -1, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	used := make([]int64, len(charges))
	denied := -1
	for index, charge := range charges {
		used[index] = b.counters[charge.Key].used
		if denied == -1 && charge.Limit > 0 && charge.Amount > charge.Limit-used[index] {
			denied = index
		}
	}
	if denied >= 0 {
		return used, denied, nil
	}
	for index, charge := range charges {
		used[index] = min(maxCounter, used[index]+charge.Amount)
		b.counters[charge.Key] = memoryCounter{used: used[index], expiresAt: charge.ExpiresAt}
	}
	return used, -1, nil
}

func (b *MemoryBackend) Adjust(ctx context.Context, charges []Charge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	for _, charge := range charges {
		if counter, exists := b.counters[charge.Key]; exists {
			counter.used = max(0, min(maxCounter, counter.used+charge.Amount))
			b.counters[charge.Key] = counter
		}
	}
	return nil
}

func (b *MemoryBackend) Counts(ctx context.Context, keys []string) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	used := make([]int64, len(keys))
	for index, key := range keys {
		used[index] = b.counters[key].used
	}
	return used, nil
}

func (b *MemoryBackend) List(ctx context.Context, prefix string) (map[string]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	used := make(map[string]int64)
	for key, counter := range b.counters {
		if strings.HasPrefix(key, prefix) {
			used[key] = counter.used
		}
	}
	return used, nil
}
