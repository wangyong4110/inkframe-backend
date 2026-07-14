package ai

import (
	"testing"
	"time"
)

func TestCircuitBreaker_AllowsWhenClosed(t *testing.T) {
	cb := NewCircuitBreaker("test", 3, 50*time.Millisecond)
	if !cb.Allow() {
		t.Fatal("expected Allow() true on fresh circuit breaker")
	}
	if err := cb.Err(); err != nil {
		t.Fatalf("expected nil Err() on fresh circuit breaker, got %v", err)
	}
}

func TestCircuitBreaker_OpensAfterThresholdFailures(t *testing.T) {
	cb := NewCircuitBreaker("test", 3, time.Hour)
	cb.RecordFailure()
	cb.RecordFailure()
	if !cb.Allow() {
		t.Fatal("expected circuit still closed below threshold")
	}
	cb.RecordFailure() // 3rd failure hits threshold
	if cb.Allow() {
		t.Fatal("expected circuit open after reaching threshold")
	}
	if err := cb.Err(); err == nil {
		t.Fatal("expected non-nil Err() when circuit open")
	}
}

func TestCircuitBreaker_StaysOpenBelowResetTimeout(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, time.Hour)
	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("expected circuit open immediately after threshold failure")
	}
}

func TestCircuitBreaker_AutoResetsAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, 20*time.Millisecond)
	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("expected circuit open right after failure")
	}
	time.Sleep(30 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("expected circuit to auto-reset (half-open probe allowed) after resetTimeout elapsed")
	}
}

func TestCircuitBreaker_RecordSuccessClosesAndResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker("test", 2, time.Hour)
	cb.RecordFailure()
	cb.RecordSuccess()
	// failures counter should be back to 0, so one more failure alone must not open it
	cb.RecordFailure()
	if !cb.Allow() {
		t.Fatal("expected circuit still closed: RecordSuccess should have reset the failure count")
	}
}

func TestCircuitBreaker_ResetForceClosesCircuit(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, time.Hour)
	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("expected circuit open before Reset")
	}
	cb.Reset()
	if !cb.Allow() {
		t.Fatal("expected circuit closed after Reset")
	}
	if err := cb.Err(); err != nil {
		t.Fatalf("expected nil Err() after Reset, got %v", err)
	}
}

func TestCircuitBreaker_ThresholdNotYetReachedKeepsClosed(t *testing.T) {
	cb := NewCircuitBreaker("test", 5, time.Hour)
	for i := 0; i < 4; i++ {
		cb.RecordFailure()
	}
	if !cb.Allow() {
		t.Fatal("expected circuit closed with failures below threshold")
	}
}

func TestCircuitBreaker_ReopensAfterFailureFollowingAutoReset(t *testing.T) {
	cb := NewCircuitBreaker("test", 1, 15*time.Millisecond)
	cb.RecordFailure()
	time.Sleep(20 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("expected auto-reset to allow probe request")
	}
	// probe fails again -> should reopen immediately (threshold=1)
	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("expected circuit to reopen after probe failure")
	}
}
