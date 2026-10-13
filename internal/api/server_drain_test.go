package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestServerDrainWaitsAndRejectsAdmission(t *testing.T) {
	server := &Server{}
	entered, release, requestDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := server.trackAdmission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	go func() {
		defer close(requestDone)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drainDone := make(chan error, 1)
	go func() { drainDone <- server.Drain(ctx) }()
	for {
		server.admission.mu.Lock()
		stopping := server.admission.stopping
		server.admission.mu.Unlock()
		if stopping {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("drain did not stop admission")
		default:
			runtime.Gosched()
		}
	}
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, httptest.NewRequest("GET", "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatal("request admitted after drain")
	}
	select {
	case <-drainDone:
		t.Fatal("drain returned before handler result")
	default:
	}
	close(release)
	<-requestDone
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
}

func TestServerDrainReportsIncompleteHandlers(t *testing.T) {
	server := &Server{}
	done := make(chan struct{})
	server.admission.active, server.admission.idle = 1, done
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("incomplete drain reported as success: %v", err)
	}
}
