package router

import "time"

// CircuitSnapshot also includes configured providers that have not been called.
func (r *Router) CircuitSnapshot() map[string]CircuitSnapshot {
	r.mu.Lock()
	breaker := r.breakers
	r.mu.Unlock()
	out := map[string]CircuitSnapshot{}
	closed := CircuitSnapshot{State: CircuitClosed}
	if breaker != nil {
		out = breaker.Snapshot()
		closed = breaker.closedSnapshot()
	}
	for _, name := range r.registry.Names() {
		if _, exists := out[name]; !exists {
			out[name] = closed
		}
	}
	return out
}

// ResetCircuit refuses unknown providers; names use Registry's normalization.
func (r *Router) ResetCircuit(name string) error {
	backend, err := r.registry.Get(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	breaker := r.breakers
	r.mu.Unlock()
	if breaker != nil {
		breaker.Reset(backend.Name())
	}
	return nil
}

func (r *Router) LatencyAverage(providerName string) time.Duration {
	return r.latency.average(providerName)
}
