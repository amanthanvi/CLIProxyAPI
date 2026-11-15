package auth

import (
	"context"
	"net/http"
	"sync"
)

type affinityExecutionDrain struct {
	mu       sync.Mutex
	stopping bool
	active   int
	idle     chan struct{}
}

// EnableAffinityDrainTracking opts into tracking native execution and stream result writers.
func (m *Manager) EnableAffinityDrainTracking() {
	if m != nil {
		m.affinityDrain.CompareAndSwap(nil, &affinityExecutionDrain{})
	}
}

func (m *Manager) beginAffinityExecution(inherited bool) (func(), error) {
	tracker := m.affinityDrain.Load()
	if tracker == nil {
		return func() {}, nil
	}
	tracker.mu.Lock()
	if tracker.stopping && !inherited {
		tracker.mu.Unlock()
		return nil, &Error{Code: "service_draining", Message: "service shutting down", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
	}
	if tracker.active == 0 {
		tracker.idle = make(chan struct{})
	}
	tracker.active++
	tracker.mu.Unlock()
	return func() {
		tracker.mu.Lock()
		tracker.active--
		if tracker.active == 0 {
			close(tracker.idle)
		}
		tracker.mu.Unlock()
	}, nil
}

// BeginAffinityDrain rejects new executions without cancelling established provider work.
func (m *Manager) BeginAffinityDrain() {
	if m == nil {
		return
	}
	if tracker := m.affinityDrain.Load(); tracker != nil {
		tracker.mu.Lock()
		tracker.stopping = true
		tracker.mu.Unlock()
	}
}

func (m *Manager) WaitAffinityIdle(ctx context.Context) error {
	if m == nil {
		return nil
	}
	tracker := m.affinityDrain.Load()
	if tracker == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tracker.mu.Lock()
	active, idle := tracker.active, tracker.idle
	tracker.mu.Unlock()
	if active == 0 {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
