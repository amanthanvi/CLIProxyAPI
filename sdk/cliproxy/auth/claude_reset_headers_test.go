package auth

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func claudeResetFixture(now time.Time, delay time.Duration) http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
		"Anthropic-Ratelimit-Unified-Reset":                {strconv.FormatInt(now.Add(delay).Unix(), 10)},
	}
}

func TestClaudeResetHeadersValidateNativeMetadata(t *testing.T) {
	now := time.Unix(1800000000, 0)
	input := claudeResetFixture(now, time.Hour)
	input.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	input.Set("Set-Cookie", "secret")
	input.Set("Authorization", "secret")
	input.Set("X-Private", "secret")
	input.Set("Anthropic-Ratelimit-Unified-Private", "secret")
	got := FilterClaudeRateLimitResponseHeaders(input, now)
	if len(got) != 4 || got.Get("Anthropic-Ratelimit-Unified-Reset") != input.Get("Anthropic-Ratelimit-Unified-Reset") {
		t.Fatalf("validated headers = %v", got)
	}
	for _, name := range []string{"Set-Cookie", "Authorization", "X-Private", "Anthropic-Ratelimit-Unified-Private"} {
		if got.Get(name) != "" {
			t.Fatalf("reflected unsafe header %s", name)
		}
	}
	got.Set("Anthropic-Ratelimit-Unified-Reset", "changed")
	if input.Get("Anthropic-Ratelimit-Unified-Reset") == "changed" {
		t.Fatal("header map aliases upstream")
	}
}

func TestClaudeResetHeadersRejectUnknownOrContradictoryMetadata(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for name, edit := range map[string]func(http.Header){
		"expired": func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(now.Unix(), 10)) },
		"unbounded": func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(now.Add(8*24*time.Hour).Unix(), 10))
		},
		"invalid": func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Reset", "NaN") },
		"oversized": func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-Reset", "1800000000000000000000000000000000000000000")
		},
		"crlf":              func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Reset", "1800003600\r\nX-Secret: value") },
		"unknown-claim":     func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "secret-value") },
		"missing-claim":     func(h http.Header) { h.Del("Anthropic-Ratelimit-Unified-Representative-Claim") },
		"missing-reset":     func(h http.Header) { h.Del("Anthropic-Ratelimit-Unified-Reset"); h.Set("Retry-After", "3600") },
		"allowed":           func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Status", "allowed") },
		"unknown-overage":   func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "secret-value") },
		"duplicate-overage": func(h http.Header) { h["Anthropic-Ratelimit-Unified-Overage-Status"] = []string{"allowed", "rejected"} },
		"duplicate-casing": func(h http.Header) {
			h["anthropic-ratelimit-unified-reset"] = []string{h.Get("Anthropic-Ratelimit-Unified-Reset")}
		},
		"contradictory-status": func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed") },
		"unknown-status":       func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-7d-Status", "secret-value") },
		"contradictory-window": func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10))
		},
		"billing-boundary": func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "overage")
			h.Set("Anthropic-Ratelimit-Unified-Overage-Reset", h.Get("Anthropic-Ratelimit-Unified-Reset"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := claudeResetFixture(now, time.Hour)
			edit(h)
			if got := FilterClaudeRateLimitResponseHeaders(h, now); len(got) != 0 {
				t.Fatalf("unexpected metadata %v", got)
			}
		})
	}
}

func TestClaudeResetSchedulerRespectsCandidatePredicateAndProvenance(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a := resetAuthFixture("later", now, 2*time.Hour, 2*time.Hour+20*time.Second)
	b := resetAuthFixture("earlier", now, time.Hour, time.Hour+25*time.Second)
	b.Attributes = map[string]string{"priority": "-10"}
	banned := resetAuthFixture("banned", now, time.Minute, 2*time.Minute)
	disabled := resetAuthFixture("disabled", now, time.Second, time.Minute)
	disabled.Disabled = true
	m := &modelScheduler{modelKey: "opus", entries: map[string]*scheduledAuth{}}
	for _, auth := range []*Auth{a, b, banned, disabled} {
		state := scheduledStateCooldown
		if auth.Disabled {
			state = scheduledStateDisabled
		}
		m.entries[auth.ID] = &scheduledAuth{auth: auth, state: state, nextRetryAt: auth.ModelStates["opus"].NextRetryAfter}
	}
	predicate := func(entry *scheduledAuth) bool { return entry.auth.ID != "banned" && !entry.auth.Disabled }
	err := m.unavailableErrorLocked("claude", "opus", predicate)
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("error=%T", err)
	}
	want := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	if cooldown.ClaudeRateLimitResponseHeaders().Get("Anthropic-Ratelimit-Unified-Reset") != want {
		t.Fatalf("did not select earliest eligible metadata: %v", cooldown.ClaudeRateLimitResponseHeaders())
	}
	// A later account's valid reset cannot substitute for an unknown earlier candidate.
	b.ModelStates["opus"].Quota.Signals = nil
	err = m.unavailableErrorLocked("claude", "opus", predicate)
	if !errors.As(err, &cooldown) || len(cooldown.ClaudeRateLimitResponseHeaders()) != 0 {
		t.Fatal("substituted later account reset")
	}
}

func TestClaudeResetManagerAndModelRestorationPreserveMetadata(t *testing.T) {
	now := time.Unix(1800000000, 0)
	manager := NewManager(nil, nil, nil)
	auth := resetAuthFixture("fixture", now, time.Hour, time.Hour+25*time.Second)
	_, err := manager.availableAuthsForRouteModel([]*Auth{auth}, "claude", "opus", now)
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) || len(cooldown.ClaudeRateLimitResponseHeaders()) != 3 {
		t.Fatalf("manager reset metadata missing: %T", err)
	}
	cooldown.model = ""
	restored := restoreModelCooldownErrorModel(cooldown, "logical/opus")
	var actual *modelCooldownError
	if !errors.As(restored, &actual) || actual.model != "logical/opus" || actual.ClaudeRateLimitResponseHeaders().Get("Anthropic-Ratelimit-Unified-Reset") != cooldown.ClaudeRateLimitResponseHeaders().Get("Anthropic-Ratelimit-Unified-Reset") {
		t.Fatal("model restoration lost reset or route")
	}
}

func TestClaudeResetSnapshotCannotLaunderAmbiguousOrInvalidHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for name, edit := range map[string]func(http.Header){
		"multivalue-reset": func(h http.Header) {
			h["Anthropic-Ratelimit-Unified-Reset"] = []string{strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10), strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}
		},
		"case-duplicate": func(h http.Header) {
			h["anthropic-ratelimit-unified-reset"] = []string{h.Get("Anthropic-Ratelimit-Unified-Reset")}
		},
		"invalid-optional-overage": func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed\r\nX-Private: secret")
		},
		"whitespace-status": func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-Status", " rejected ") },
		"whitespace-status-key": func(h http.Header) {
			delete(h, "Anthropic-Ratelimit-Unified-Status")
			h[" Anthropic-Ratelimit-Unified-Status "] = []string{"rejected"}
		},
		"whitespace-overage-key": func(h http.Header) { h[" Anthropic-Ratelimit-Unified-Overage-Status "] = []string{"allowed"} },
	} {
		t.Run(name, func(t *testing.T) {
			headers := claudeResetFixture(now, time.Hour)
			edit(headers)
			if len(FilterClaudeRateLimitResponseHeaders(headers, now)) != 0 {
				t.Fatal("original metadata was not rejected")
			}
			auth := resetAuthFixture("snapshot", now, time.Hour, time.Hour+25*time.Second)
			quota := &auth.ModelStates["opus"].Quota
			quota.ObserveResponseHeadersForProvider("claude", headers, now)
			if len(claudeResetHeadersForAuth(auth, "opus", now)) != 0 {
				t.Fatal("lossy observation manufactured valid reset provenance")
			}
		})
	}
}

func resetAuthFixture(id string, now time.Time, nativeDelay, schedulerDelay time.Duration) *Auth {
	h := claudeResetFixture(now, nativeDelay)
	signals := map[string]string{}
	for name := range h {
		signals[name] = h.Get(name)
	}
	return &Auth{ID: id, Provider: "claude", UpdatedAt: now, ModelStates: map[string]*ModelState{
		"opus": {Unavailable: true, Status: StatusError, UpdatedAt: now,
			LastError: &Error{HTTPStatus: 429}, NextRetryAfter: now.Add(schedulerDelay),
			Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(schedulerDelay), ObservedAt: now, Signals: signals}},
	}}
}

func TestClaudeResetPoolUsesEarliestEligibleExactModelProvenance(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := resetAuthFixture("later", now, 2*time.Hour, 2*time.Hour+20*time.Second)
	b := resetAuthFixture("earlier", now, time.Hour, time.Hour+25*time.Second)
	b.Attributes = map[string]string{"priority": "-10"}
	_, err := getAvailableAuths([]*Auth{a, b}, "claude", "opus", now)
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("error = %T", err)
	}
	got := cooldown.ClaudeRateLimitResponseHeaders()
	want := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	if got.Get("Anthropic-Ratelimit-Unified-Reset") != want {
		t.Fatalf("reset = %v, want native %s rather than scheduler jitter", got, want)
	}
	if cooldown.resetIn != time.Hour+25*time.Second {
		t.Fatalf("scheduler policy changed: %s", cooldown.resetIn)
	}
	for name, edit := range map[string]func(*Auth){
		"other-provider":      func(a *Auth) { a.Provider = "codex" },
		"missing-observation": func(a *Auth) { a.ModelStates["opus"].Quota.ObservedAt = time.Time{} },
		"stale-observation":   func(a *Auth) { a.ModelStates["opus"].UpdatedAt = now.Add(time.Second) },
		"other-model": func(a *Auth) {
			a.ModelStates["sonnet"] = a.ModelStates["opus"]
			a.ModelStates["opus"].Quota.Signals = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := resetAuthFixture("one", now, time.Hour, time.Hour+25*time.Second)
			edit(candidate)
			_, err := getAvailableAuths([]*Auth{candidate}, "claude", "opus", now)
			if !errors.As(err, &cooldown) {
				t.Fatalf("error=%T", err)
			}
			if len(cooldown.ClaudeRateLimitResponseHeaders()) != 0 {
				t.Fatal("reported reset without eligible exact-model provenance")
			}
		})
	}
}
