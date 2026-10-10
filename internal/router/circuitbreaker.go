package router

import (
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half-open"
)

type CircuitBreaker struct {
	mu        sync.Mutex
	entries   map[string]*circuitEntry
	threshold int
	cooldown  time.Duration
}

type circuitEntry struct {
	state       CircuitState
	failures    int
	openUntil   time.Time
	lastFailure time.Time
}

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}
	return &CircuitBreaker{
		entries:   map[string]*circuitEntry{},
		threshold: threshold,
		cooldown:  cooldown,
	}
}

func (b *CircuitBreaker) Allow(providerName string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entry(providerName)
	if entry.state != CircuitOpen {
		return true
	}
	if time.Now().After(entry.openUntil) {
		entry.state = CircuitHalfOpen
		return true
	}
	return false
}

func (b *CircuitBreaker) RecordSuccess(providerName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entry(providerName)
	entry.state = CircuitClosed
	entry.failures = 0
	entry.openUntil = time.Time{}
}

func (b *CircuitBreaker) RecordFailure(providerName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entry(providerName)
	entry.failures++
	entry.lastFailure = time.Now()
	if entry.failures >= b.threshold {
		entry.state = CircuitOpen
		entry.openUntil = time.Now().Add(b.cooldown)
	}
}

func (b *CircuitBreaker) State(providerName string) CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entry(providerName).state
}

func (b *CircuitBreaker) entry(providerName string) *circuitEntry {
	entry, ok := b.entries[providerName]
	if !ok {
		entry = &circuitEntry{state: CircuitClosed}
		b.entries[providerName] = entry
	}
	return entry
}

// CircuitSnapshot is a detached, read-only view; taking a snapshot never
// advances the state machine or consumes a half-open probe.
type CircuitSnapshot struct {
	State       CircuitState `json:"state"`
	Failures    int          `json:"failures"`
	Threshold   int          `json:"threshold"`
	CooldownS   float64      `json:"cooldown_s"`
	OpenUntil   *time.Time   `json:"open_until"`
	LastFailure *time.Time   `json:"last_failure"`
}

func (b *CircuitBreaker) Snapshot() map[string]CircuitSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]CircuitSnapshot, len(b.entries))
	for name, entry := range b.entries {
		value := CircuitSnapshot{State: entry.state, Failures: entry.failures, Threshold: b.threshold, CooldownS: b.cooldown.Seconds()}
		if !entry.openUntil.IsZero() {
			at := entry.openUntil.UTC()
			value.OpenUntil = &at
		}
		if !entry.lastFailure.IsZero() {
			at := entry.lastFailure.UTC()
			value.LastFailure = &at
		}
		out[name] = value
	}
	return out
}

func (b *CircuitBreaker) closedSnapshot() CircuitSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return CircuitSnapshot{State: CircuitClosed, Threshold: b.threshold, CooldownS: b.cooldown.Seconds()}
}

// Reset clears failure history for a provider. Normal Allow/Record logic is
// unchanged; an operator must explicitly invoke this administrative action.
func (b *CircuitBreaker) Reset(providerName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[providerName] = &circuitEntry{state: CircuitClosed}
}
