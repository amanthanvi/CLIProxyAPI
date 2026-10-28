package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type poolQuotaTestError struct{}

func (*poolQuotaTestError) Error() string              { return "synthetic quota" }
func (*poolQuotaTestError) StatusCode() int            { return 429 }
func (*poolQuotaTestError) RetryAfter() *time.Duration { d := time.Hour; return &d }
func (*poolQuotaTestError) Headers() http.Header {
	return http.Header{"Retry-After": []string{"3600"}, "X-Synthetic": []string{"preserved"}}
}

type poolQuotaTestExecutor struct {
	failure             error
	bootstrap           bool
	provider            string
	outputBeforeFailure bool
}

func (e *poolQuotaTestExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "codex"
}
func (e *poolQuotaTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.failure
}
func (e *poolQuotaTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.outputBeforeFailure {
		ch := make(chan cliproxyexecutor.StreamChunk, 2)
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic output\"}\n\n")}
		ch <- cliproxyexecutor.StreamChunk{Err: e.failure}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	if !e.bootstrap {
		return nil, e.failure
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: e.failure}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}
func (*poolQuotaTestExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (*poolQuotaTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*poolQuotaTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	panic("network forbidden")
}

func newPoolQuotaTestManager(t *testing.T) (*Manager, []string) {
	t.Helper()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetRetryConfig(0, 0, 0)
	m.RegisterExecutor(&poolQuotaTestExecutor{failure: &poolQuotaTestError{}})
	ids := []string{t.Name() + "-a", t.Name() + "-b"}
	for _, id := range ids {
		if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "quota-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, ids
}

func parkPoolQuotaTestManager(m *Manager, ids []string) {
	for _, id := range ids {
		d := time.Hour
		m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "quota-model", RouteModel: "quota-model", Error: &Error{HTTPStatus: 429, Message: "synthetic quota"}, RetryAfter: &d})
	}
}

func TestPoolQuotaExhaustionExecutionBoundary(t *testing.T) {
	for _, mode := range []string{"execute-last-429", "stream-last-429", "stream-bootstrap", "execute-already-parked", "stream-already-parked"} {
		t.Run(mode, func(t *testing.T) {
			m, ids := newPoolQuotaTestManager(t)
			original := &poolQuotaTestError{}
			m.RegisterExecutor(&poolQuotaTestExecutor{failure: original, bootstrap: mode == "stream-bootstrap"})
			if mode == "execute-already-parked" || mode == "stream-already-parked" {
				parkPoolQuotaTestManager(m, ids)
			}
			req := cliproxyexecutor.Request{Model: "quota-model"}
			var err error
			if mode == "execute-last-429" || mode == "execute-already-parked" {
				_, err = m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
			} else {
				var stream *cliproxyexecutor.StreamResult
				stream, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			details, ok := PoolQuotaExhaustionDetails(err)
			if !ok {
				t.Fatalf("whole-pool quota error was not certified: %T", err)
			}
			if details.EligibleCredentials != 2 || details.ObservedAt.IsZero() || !details.EarliestAdmission.After(details.ObservedAt) {
				t.Fatalf("invalid local observation: %+v", details)
			}
			if mode == "execute-last-429" || mode == "stream-last-429" {
				if !errors.Is(err, original) {
					t.Fatal("original upstream error was lost")
				}
			}
		})
	}
}

func TestPoolQuotaExhaustionFreshFullPoolScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Manager, []string, *cliproxyexecutor.Options) context.Context
		want   bool
	}{
		{"all-quota", nil, true},
		{"healthy-low-priority", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			_, _, _ = m.ResetQuota(WithSkipPersist(context.Background()), ids[1])
			m.mu.Lock()
			m.auths[ids[1]].Attributes = map[string]string{"priority": "-10"}
			m.mu.Unlock()
			return nil
		}, false},
		{"healthy-tried", func(m *Manager, ids []string, o *cliproxyexecutor.Options) context.Context {
			_, _, _ = m.ResetQuota(WithSkipPersist(context.Background()), ids[1])
			*o = withAttemptedAuthTracker(*o, map[string]struct{}{ids[1]: {}})
			return nil
		}, false},
		{"disabled-excluded", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			m.mu.Lock()
			m.auths[ids[1]].Disabled = true
			m.mu.Unlock()
			return nil
		}, true},
		{"unsupported-excluded", func(_ *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			registry.GetGlobalRegistry().UnregisterClient(ids[1])
			return nil
		}, true},
		{"transient-model", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			m.mu.Lock()
			s := m.auths[ids[1]].ModelStates["quota-model"]
			s.Quota = QuotaState{}
			s.LastError = &Error{HTTPStatus: 503}
			m.mu.Unlock()
			return nil
		}, false},
		{"auth-error", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			m.mu.Lock()
			m.auths[ids[1]].LastError = &Error{HTTPStatus: 401}
			m.mu.Unlock()
			return nil
		}, false},
		{"expired", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			expireSessionAffinityPriorityModelCooldown(t, m, ids[1], "quota-model")
			return nil
		}, false},
		{"unknown-deadline", func(m *Manager, ids []string, _ *cliproxyexecutor.Options) context.Context {
			m.mu.Lock()
			s := m.auths[ids[1]].ModelStates["quota-model"]
			s.NextRetryAfter = time.Time{}
			s.Quota.NextRecoverAt = time.Time{}
			m.mu.Unlock()
			return nil
		}, false},
		{"pinned", func(_ *Manager, ids []string, o *cliproxyexecutor.Options) context.Context {
			o.Metadata = map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: ids[0]}
			return nil
		}, false},
		{"home", func(m *Manager, _ []string, _ *cliproxyexecutor.Options) context.Context {
			m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			return nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ids := newPoolQuotaTestManager(t)
			parkPoolQuotaTestManager(m, ids)
			opts := cliproxyexecutor.Options{}
			ctx := context.Background()
			if tc.change != nil {
				if c := tc.change(m, ids, &opts); c != nil {
					ctx = c
				}
			}
			cause := &poolQuotaTestError{}
			err := m.certifyPoolQuotaExhaustion(ctx, []string{"codex"}, "quota-model", opts, cause)
			_, ok := PoolQuotaExhaustionDetails(err)
			if ok != tc.want {
				t.Fatalf("certified=%t want %t", ok, tc.want)
			}
		})
	}
}

type poolQuotaUnknownSelector struct{ called bool }

func (s *poolQuotaUnknownSelector) Pick(context.Context, string, string, cliproxyexecutor.Options, []*Auth) (*Auth, error) {
	s.called = true
	return nil, nil
}

func TestPoolQuotaExhaustionCanonicalAffinityAliasAndPreservation(t *testing.T) {
	m, ids := newPoolQuotaTestManager(t)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: 24 * time.Hour, RebindOn: SessionAffinityRebindOnQuota})
	t.Cleanup(affinity.Stop)
	m.SetSelector(affinity)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": {{Name: "quota-model", Alias: "public-model"}}})
	m.mu.Lock()
	for _, id := range ids {
		m.auths[id].Prefix = "codex"
	}
	m.mu.Unlock()
	parkPoolQuotaTestManager(m, ids)
	snapshot, err := m.SnapshotAffinity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RestoreAffinity(snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	cause := &poolQuotaTestError{}
	err = m.certifyPoolQuotaExhaustion(context.Background(), []string{"codex"}, "codex/public-model(xhigh)", cliproxyexecutor.Options{}, cause)
	details, ok := PoolQuotaExhaustionDetails(err)
	if !ok || details.EligibleCredentials != 2 {
		t.Fatal("canonical affinity/alias route was not certified")
	}
	if !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatal("cause identity or message changed")
	}
	if statusCodeFromError(err) != 429 {
		t.Fatal("cause status changed")
	}
	var withHeaders interface{ Headers() http.Header }
	if !errors.As(err, &withHeaders) || withHeaders.Headers().Get("X-Synthetic") != "preserved" {
		t.Fatal("cause headers changed")
	}
	if d := retryAfterFromError(err); d == nil || *d != time.Hour {
		t.Fatal("cause retry metadata changed")
	}
	details.EligibleCredentials = 99
	fresh, _ := PoolQuotaExhaustionDetails(err)
	if fresh.EligibleCredentials != 2 {
		t.Fatal("certificate was externally mutable")
	}
	for _, id := range ids {
		_, _, _ = m.ResetQuota(WithSkipPersist(context.Background()), id)
	}
	if _, ok := PoolQuotaExhaustionDetails(m.certifyPoolQuotaExhaustion(context.Background(), []string{"codex"}, "codex/public-model(xhigh)", cliproxyexecutor.Options{}, cause)); ok {
		t.Fatal("reset pool retained certification")
	}
}

func TestPoolQuotaExhaustionConservativeGuards(t *testing.T) {
	for _, name := range []string{"custom-selector", "request-scoped", "safety-stop", "missing-executor", "canceled", "mixed-credential-model", "zero-eligible", "eligibility-filter"} {
		t.Run(name, func(t *testing.T) {
			m, ids := newPoolQuotaTestManager(t)
			parkPoolQuotaTestManager(m, ids)
			ctx := context.Background()
			var cause error = &poolQuotaTestError{}
			want := false
			var custom *poolQuotaUnknownSelector
			switch name {
			case "custom-selector":
				custom = &poolQuotaUnknownSelector{}
				m.SetSelector(custom)
			case "request-scoped":
				cause = &Error{Code: ErrorCodeRequestScoped, HTTPStatus: 429, Message: "synthetic caller fault"}
			case "safety-stop":
				cause = wrapRequestStopError(&Error{HTTPStatus: 403, Message: "synthetic safety stop"})
			case "missing-executor":
				m.mu.Lock()
				delete(m.executors, "codex")
				m.mu.Unlock()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "mixed-credential-model":
				m.mu.Lock()
				a := m.auths[ids[0]]
				a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
				a.ModelStates["quota-model"].LastError = &Error{HTTPStatus: 503}
				m.mu.Unlock()
			case "zero-eligible":
				for _, id := range ids {
					registry.GetGlobalRegistry().UnregisterClient(id)
				}
			case "eligibility-filter":
				_, _, _ = m.ResetQuota(WithSkipPersist(ctx), ids[1])
				m.mu.Lock()
				m.auths[ids[1]].Attributes = map[string]string{"api_key": "synthetic"}
				m.mu.Unlock()
				ctx = withRequiredAuthKind(ctx, AuthKindOAuth)
				want = true
			}
			err := m.certifyPoolQuotaExhaustion(ctx, []string{"codex"}, "quota-model", cliproxyexecutor.Options{}, cause)
			details, ok := PoolQuotaExhaustionDetails(err)
			if ok != want {
				t.Fatalf("certified=%t want%t", ok, want)
			}
			if want && details.EligibleCredentials != 1 {
				t.Fatal("request eligibility was ignored")
			}
			if custom != nil && custom.called {
				t.Fatal("certification invoked stateful selector")
			}
		})
	}
}

func TestPoolQuotaExhaustionRejectsAmbiguousAPIKeyModelPool(t *testing.T) {
	m, ids := newPoolQuotaTestManager(t)
	m.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: "pool-provider", Models: []internalconfig.OpenAICompatibilityModel{{Name: "upstream-a", Alias: "quota-model"}, {Name: "upstream-b", Alias: "quota-model"}}}}})
	m.mu.Lock()
	for _, id := range ids {
		m.auths[id].Attributes = map[string]string{"api_key": "synthetic", "compat_name": "pool-provider"}
	}
	provider := executorKeyFromAuth(m.auths[ids[0]])
	m.mu.Unlock()
	m.RegisterExecutor(&poolQuotaTestExecutor{provider: provider, failure: &poolQuotaTestError{}})
	parkPoolQuotaTestManager(m, ids)
	if pool := m.resolveOpenAICompatUpstreamModelPool(m.List()[0], "quota-model"); len(pool) != 2 {
		t.Fatalf("ambiguous fixture has%d upstreams", len(pool))
	}
	if _, ok := PoolQuotaExhaustionDetails(m.certifyPoolQuotaExhaustion(context.Background(), []string{provider}, "quota-model", cliproxyexecutor.Options{}, &poolQuotaTestError{})); ok {
		t.Fatal("ambiguous upstream pool was certified")
	}
	m.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: "pool-provider", Models: []internalconfig.OpenAICompatibilityModel{{Name: "upstream-a", Alias: "quota-model"}}}}})
	if _, ok := PoolQuotaExhaustionDetails(m.certifyPoolQuotaExhaustion(context.Background(), []string{provider}, "quota-model", cliproxyexecutor.Options{}, &poolQuotaTestError{})); !ok {
		t.Fatal("unambiguous single-upstream API-key route was not certified")
	}
}

func TestPoolQuotaExhaustionAdmissionAndSpoofing(t *testing.T) {
	m, ids := newPoolQuotaTestManager(t)
	parkPoolQuotaTestManager(m, ids)
	first := time.Now().Add(10 * time.Minute)
	second := first.Add(10 * time.Minute)
	m.mu.Lock()
	for i, id := range ids {
		deadline := first
		if i == 1 {
			deadline = second
		}
		s := m.auths[id].ModelStates["quota-model"]
		s.Quota.NextRecoverAt = deadline
		s.NextRetryAfter = deadline
	}
	m.mu.Unlock()
	err := m.certifyPoolQuotaExhaustion(context.Background(), []string{"codex"}, "quota-model", cliproxyexecutor.Options{}, &poolQuotaTestError{})
	details, ok := PoolQuotaExhaustionDetails(err)
	if !ok || !details.EarliestAdmission.Equal(first) {
		t.Fatalf("admission=%v want%v", details.EarliestAdmission, first)
	}
	if _, ok := PoolQuotaExhaustionDetails(errors.New(`{"error":{"code":"model_cooldown","type":"usage_limit_reached"}}`)); ok {
		t.Fatal("JSON spoofed pool certification")
	}
	if _, ok := PoolQuotaExhaustionDetails(NewModelCooldownError("quota-model", "codex", time.Hour)); ok {
		t.Fatal("plain cooldown marker spoofed full-pool certification")
	}
}

func TestPoolQuotaExhaustionDoesNotCertifyAfterOutput(t *testing.T) {
	m, ids := newPoolQuotaTestManager(t)
	parkPoolQuotaTestManager(m, ids[:1])
	m.RegisterExecutor(&poolQuotaTestExecutor{failure: &poolQuotaTestError{}, outputBeforeFailure: true})
	stream, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "quota-model"}, cliproxyexecutor.Options{})
	if err != nil || stream == nil {
		t.Fatalf("expected started stream, got %v", err)
	}
	sawOutput, sawError := false, false
	for chunk := range stream.Chunks {
		if len(chunk.Payload) > 0 {
			sawOutput = true
		}
		if chunk.Err != nil {
			sawError = true
			if _, ok := PoolQuotaExhaustionDetails(chunk.Err); ok {
				t.Fatal("post-output error was certified")
			}
		}
	}
	if !sawOutput || !sawError {
		t.Fatal("post-output fixture did not exercise both chunks")
	}
}
