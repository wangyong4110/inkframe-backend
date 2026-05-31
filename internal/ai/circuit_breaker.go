package ai

import (
	"fmt"
	"sync"
	"time"
)

// CircuitBreaker is a simple circuit breaker that opens after a threshold of consecutive
// failures and resets after a timeout.
type CircuitBreaker struct {
	mu           sync.Mutex
	failures     int
	lastFailure  time.Time
	threshold    int
	resetTimeout time.Duration
	open         bool
}

// NewCircuitBreaker creates a new CircuitBreaker with the given failure threshold and reset timeout.
func NewCircuitBreaker(threshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{threshold: threshold, resetTimeout: resetTimeout}
}

// Allow returns true if the circuit is closed (requests are allowed).
// If the circuit is open but the reset timeout has elapsed, it transitions back to closed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.open {
		if time.Since(cb.lastFailure) > cb.resetTimeout {
			cb.open = false
			cb.failures = 0
			return true
		}
		return false
	}
	return true
}

// RecordSuccess resets the failure counter and closes the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.open = false
}

// RecordFailure increments the failure counter and opens the circuit if threshold is reached.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure = time.Now()
	if cb.failures >= cb.threshold {
		cb.open = true
	}
}

// Err returns an error if the circuit is open, or nil if requests are allowed.
func (cb *CircuitBreaker) Err() error {
	if !cb.Allow() {
		return fmt.Errorf("circuit breaker open: provider temporarily unavailable")
	}
	return nil
}
