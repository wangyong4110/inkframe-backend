package ai

import (
	"fmt"
	"sync"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// CircuitBreaker is a simple circuit breaker that opens after a threshold of consecutive
// failures and resets after a timeout.
type CircuitBreaker struct {
	mu           sync.Mutex
	name         string // provider name, for logging
	failures     int
	lastFailure  time.Time
	threshold    int
	resetTimeout time.Duration
	open         bool
}

// NewCircuitBreaker creates a new CircuitBreaker with the given failure threshold and reset timeout.
func NewCircuitBreaker(name string, threshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{name: name, threshold: threshold, resetTimeout: resetTimeout}
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
			logger.Printf("[CircuitBreaker] provider=%s auto-reset after %s, allowing probe request", cb.name, cb.resetTimeout)
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
	if cb.failures > 0 || cb.open {
		logger.Printf("[CircuitBreaker] provider=%s recovered (failures reset from %d)", cb.name, cb.failures)
	}
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
		if !cb.open {
			logger.Errorf("[CircuitBreaker] provider=%s OPENED after %d consecutive failures, blocking for %s",
				cb.name, cb.failures, cb.resetTimeout)
		}
		cb.open = true
	} else {
		logger.Warnf("[CircuitBreaker] provider=%s failure %d/%d", cb.name, cb.failures, cb.threshold)
	}
}

// Reset force-closes the circuit and clears the failure counter.
// Call this when an external probe (health check) confirms the provider is healthy.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.open {
		logger.Printf("[CircuitBreaker] provider=%s force-reset", cb.name)
	}
	cb.open = false
	cb.failures = 0
}

// Err returns an error if the circuit is open, or nil if requests are allowed.
func (cb *CircuitBreaker) Err() error {
	if !cb.Allow() {
		return fmt.Errorf("circuit breaker open: provider temporarily unavailable")
	}
	return nil
}
