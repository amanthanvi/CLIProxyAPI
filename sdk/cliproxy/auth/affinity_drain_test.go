package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAffinityDrainWaitsForFinalStreamResult(t *testing.T) {
	hookEntered, releaseHook := make(chan struct{}), make(chan struct{})
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetResultPolicy(ResultPolicyFunc(func(_ context.Context, result Result) Result {
		close(hookEntered)
		<-releaseHook
		return result
	}))
	m.EnableAffinityDrainTracking()
	parentDone, err := m.beginAffinityExecution(false)
	if err != nil {
		t.Fatal(err)
	}
	remaining := make(chan cliproxyexecutor.StreamChunk)
	result := m.wrapStreamResult(context.Background(), &Auth{ID: "synthetic", Provider: "codex"}, "codex", "model", "model", nil, nil, remaining, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{})
	parentDone()
	m.BeginAffinityDrain()
	if _, err := m.Execute(context.Background(), nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("new execution admitted during drain")
	} else {
		var draining *Error
		if !errors.As(err, &draining) || draining.StatusCode() != http.StatusServiceUnavailable {
			t.Fatal("draining execution did not return 503")
		}
	}
	close(remaining)
	<-hookEntered
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.WaitAffinityIdle(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("result writer not tracked: %v", err)
	}
	close(releaseHook)
	for range result.Chunks {
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := m.WaitAffinityIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAffinityDrainDisabledPreservesAdmission(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.BeginAffinityDrain()
	done, err := m.beginAffinityExecution(false)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if err := m.WaitAffinityIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}
