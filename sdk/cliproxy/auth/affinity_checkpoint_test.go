package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func affinityTestManager(t *testing.T, ttl time.Duration, fallback Selector, auths []*Auth) (*Manager, *SessionAffinitySelector) {
	t.Helper()
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: ttl, RebindOn: SessionAffinityRebindOnQuota})
	t.Cleanup(s.Stop)
	m := NewManager(nil, s, nil)
	for _, a := range auths {
		if _, err := m.Register(WithSkipPersist(context.Background()), a.Clone()); err != nil {
			t.Fatal(err)
		}
	}
	return m, s
}

func checkpointTestAuths() []*Auth {
	return []*Auth{
		{ID: "account-a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"account_id": "stable-a", "email": "a@example.invalid"}},
		{ID: "account-b", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{"account_id": "stable-b"}},
	}
}

func TestAffinityCheckpointRoundTripAndEligibility(t *testing.T) {
	auths := checkpointTestAuths()
	m, s := affinityTestManager(t, time.Hour, &RoundRobinSelector{}, auths)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"native-thread"}}, Metadata: map[string]any{}}
	// A starts alone; B later recovers with a higher cold priority.
	selected, err := s.Pick(context.Background(), "codex", "model-a", opts, auths[:1])
	if err != nil || selected.ID != "account-a" {
		t.Fatalf("initial: %v %v", selected, err)
	}
	boundKey := s.cache.Snapshot(time.Now())[0].Aliases[0]
	s.cache.SetAliases("account-a", boundKey, "codex::alias-thread::model-a")
	now := time.Now()
	snapshot, err := m.SnapshotAffinity(now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stable-a") || strings.Contains(string(raw), "example.invalid") {
		t.Fatal("account data leaked")
	}
	var decoded AffinitySnapshot
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	nextManager, next := affinityTestManager(t, time.Hour, &FillFirstSelector{}, auths)
	if err = nextManager.RestoreAffinity(decoded, now); err != nil {
		t.Fatal(err)
	}
	selected, err = next.Pick(context.Background(), "codex", "model-a", opts, []*Auth{auths[1], auths[0]})
	if err != nil || selected.ID != "account-a" {
		t.Fatalf("restored selection: %v %v", selected, err)
	}
	if id, ok := next.cache.Get("codex::alias-thread::model-a"); !ok || id != "account-a" {
		t.Fatal("alias group lost")
	}
	other, err := next.Pick(context.Background(), "codex", "model-b", opts, auths)
	if err != nil || other.ID != "account-b" {
		t.Fatalf("model isolation: %v %v", other, err)
	}
	other, err = next.Pick(context.Background(), "claude", "model-a", opts, auths)
	if err != nil || other.ID != "account-b" {
		t.Fatalf("provider isolation: %v %v", other, err)
	}
	// Disabled and removed accounts are never restored.
	for _, pool := range [][]*Auth{auths[1:], {func() *Auth { a := auths[0].Clone(); a.Disabled = true; return a }(), auths[1]}} {
		mm, ss := affinityTestManager(t, time.Hour, &FillFirstSelector{}, pool)
		if err := mm.RestoreAffinity(decoded, now); err != nil {
			t.Fatal(err)
		}
		if ss.cache.Len() != 0 {
			t.Fatal("ineligible account restored")
		}
	}
}

func TestAffinityCheckpointRefreshVersusAccountReplacement(t *testing.T) {
	auths := checkpointTestAuths()
	m, s := affinityTestManager(t, time.Hour, &FillFirstSelector{}, auths)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"stable-thread"}}, Metadata: map[string]any{}}
	if _, err := s.Pick(context.Background(), "codex", "model", opts, auths[:1]); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.SnapshotAffinity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	refreshed := auths[0].Clone()
	refreshed.Metadata["access_token"] = "synthetic-refresh"
	refreshed.CredentialVersion = 55
	mm, ss := affinityTestManager(t, time.Hour, &FillFirstSelector{}, []*Auth{refreshed, auths[1]})
	if err := mm.RestoreAffinity(snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := ss.Pick(context.Background(), "codex", "model", opts, []*Auth{refreshed, auths[1]}); got.ID != "account-a" {
		t.Fatal("refresh changed identity")
	}
	replaced := refreshed.Clone()
	replaced.Metadata["account_id"] = "different-account"
	if got, _ := ss.Pick(context.Background(), "codex", "model", opts, []*Auth{replaced, auths[1]}); got.ID != "account-b" {
		t.Fatal("replacement reused stale binding")
	}
	mm2, ss2 := affinityTestManager(t, time.Hour, &FillFirstSelector{}, []*Auth{replaced, auths[1]})
	if err := mm2.RestoreAffinity(snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ss2.cache.Len() != 0 {
		t.Fatal("replacement restored stale hint")
	}
}

func TestSessionCheckpointAbsoluteExpiryBoundsAndLivePrecedence(t *testing.T) {
	now := time.Now()
	c := NewSessionCacheWithCapacity(time.Hour, 3)
	defer c.Stop()
	groups := []SessionBinding{{AuthID: "a", Aliases: []string{"one", "alias"}, ExpiresAt: now.Add(10 * time.Minute)}, {AuthID: "a", Aliases: []string{"expired"}, ExpiresAt: now.Add(-time.Second)}}
	if err := c.Restore(groups, now); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Fatal("expired group restored")
	}
	if expiry := c.Snapshot(now)[0].ExpiresAt; !expiry.Equal(groups[0].ExpiresAt) {
		t.Fatal("TTL renewed")
	}
	c.Set("one", "new")
	if err := c.Restore(groups, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("one"); got != "new" {
		t.Fatal("stale snapshot replaced live binding")
	}
	short := NewSessionCache(time.Minute)
	defer short.Stop()
	if err := short.Restore(groups, now); err != nil {
		t.Fatal(err)
	}
	if expiry := short.Snapshot(now)[0].ExpiresAt; !expiry.Equal(now.Add(time.Minute)) {
		t.Fatal("decreased TTL not clamped")
	}
	for _, bad := range [][]SessionBinding{
		{{AuthID: "a", Aliases: []string{"duplicate", "duplicate"}, ExpiresAt: now.Add(time.Minute)}},
		{{AuthID: "a", Aliases: []string{"a", "b", "c", "d"}, ExpiresAt: now.Add(time.Minute)}},
		{{AuthID: "a", Aliases: []string{strings.Repeat("x", maxAffinityIdentifierBytes+1)}, ExpiresAt: now.Add(time.Minute)}},
	} {
		if err := c.Restore(bad, now); err == nil {
			t.Fatal("invalid document accepted")
		}
	}
	full := NewSessionCacheWithCapacity(time.Hour, 1)
	defer full.Stop()
	full.Set("live", "new")
	if err := full.Restore([]SessionBinding{{AuthID: "old", Aliases: []string{"disjoint"}, ExpiresAt: now.Add(time.Minute)}}, now); err != nil {
		t.Fatal(err)
	}
	if id, ok := full.Get("live"); !ok || id != "new" {
		t.Fatal("stale restore evicted unrelated live binding")
	}
	mixed := NewSessionCacheWithCapacity(time.Hour, 2)
	defer mixed.Stop()
	mixed.Set("live", "current")
	if err := mixed.Restore([]SessionBinding{{AuthID: "old", Aliases: []string{"hint"}, ExpiresAt: now.Add(time.Minute)}}, now); err != nil {
		t.Fatal(err)
	}
	mixed.Set("new-live", "current")
	if _, ok := mixed.Get("hint"); ok {
		t.Fatal("stale hint displaced live state on ordinary capacity eviction")
	}
	for _, key := range []string{"live", "new-live"} {
		if _, ok := mixed.Get(key); !ok {
			t.Fatal("live binding evicted before stale hint")
		}
	}
}

func TestAffinityCompatibleReconfigurationAndConcurrentCheckpoint(t *testing.T) {
	auths := checkpointTestAuths()
	m, s := affinityTestManager(t, time.Hour, &RoundRobinSelector{}, auths)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"continuity"}}, Metadata: map[string]any{}}
	if _, err := s.Pick(context.Background(), "codex", "model", opts, auths[:1]); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				local := cliproxyexecutor.Options{Headers: opts.Headers, Metadata: map[string]any{}}
				if _, err := s.Pick(context.Background(), "codex", "model", local, auths); err != nil {
					t.Error(err)
				}
				s.OnResult(Result{AuthID: "account-a", Provider: "codex", Model: "model", Success: true, Options: local})
				if _, err := m.SnapshotAffinity(time.Now()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Minute, RebindOn: SessionAffinityRebindOnQuota}))
	}
	wg.Wait()
	if m.Selector() != s {
		t.Fatal("compatible reload replaced selector state")
	}
	if got, _ := s.Pick(context.Background(), "codex", "model", opts, auths); got.ID != "account-a" {
		t.Fatal("hot reload lost warm assignment")
	}
	before := s.cache.Snapshot(time.Now())[0].ExpiresAt
	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour}))
	if after := s.cache.Snapshot(time.Now())[0].ExpiresAt; !after.Equal(before) {
		t.Fatal("TTL increase renewed expiry")
	}
}

func TestAffinityCheckpointRetainsQuotaRebindAndDelayedResultProtection(t *testing.T) {
	auths := checkpointTestAuths()
	m, s := affinityTestManager(t, time.Hour, &FillFirstSelector{}, auths)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"quota-thread"}}, Metadata: map[string]any{}}
	if _, err := s.Pick(context.Background(), "codex", "model", opts, auths[:1]); err != nil {
		t.Fatal(err)
	}
	transient := auths[0].Clone()
	transient.Unavailable = true
	transient.NextRetryAfter = time.Now().Add(time.Hour)
	transient.LastError = &Error{HTTPStatus: 503}
	if got, err := s.Pick(context.Background(), "codex", "model", opts, []*Auth{transient, auths[1]}); err != nil || got.ID != "account-b" {
		t.Fatalf("transient failover: %v %v", got, err)
	}
	boundKey := "codex::" + CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata) + "::model"
	if id, _ := s.cache.Get(boundKey); id != "account-a" {
		t.Fatal("transient failure rebound session")
	}
	quota := transient.Clone()
	quota.Quota.Exceeded = true
	quota.LastError = &Error{HTTPStatus: 429}
	if got, err := s.Pick(context.Background(), "codex", "model", opts, []*Auth{quota, auths[1]}); err != nil || got.ID != "account-b" {
		t.Fatalf("quota failover: %v %v", got, err)
	}
	// Old completion must neither delete B nor revive A after real rebinding.
	s.OnResult(Result{AuthID: "account-a", Provider: "codex", Model: "model", Error: &Error{HTTPStatus: 429}, Options: opts})
	s.OnResult(Result{AuthID: "account-a", Provider: "codex", Model: "model", Success: true, Options: opts})
	snapshot, err := m.SnapshotAffinity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m2, s2 := affinityTestManager(t, time.Hour, &RoundRobinSelector{}, auths)
	if err := m2.RestoreAffinity(snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Pick(context.Background(), "codex", "model", opts, auths); err != nil || got.ID != "account-b" {
		t.Fatalf("recovery lost quota rebind: %v %v", got, err)
	}
}

func TestAffinityCheckpointByteBudgetRetainsNewestNativeCapacityHints(t *testing.T) {
	snapshot := AffinitySnapshot{Version: AffinitySnapshotVersion, Accounts: map[string]string{"a": strings.Repeat("b", 64)}}
	for i := 0; i < session.MerklePrefixSnapshotMaxGroups; i++ {
		fingerprints := make([]string, 64)
		for j := range fingerprints {
			fingerprints[j] = strings.Repeat("a", 64)
		}
		snapshot.Prefixes.Groups = append(snapshot.Prefixes.Groups, session.MerklePrefixSnapshotGroup{AuthID: "a", Namespace: fmt.Sprintf("namespace-%04d", i), SessionID: fmt.Sprintf("session-%04d", i), Fingerprints: fingerprints, AccessNumber: uint64(i + 1)})
	}
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) <= maxAffinityStateBytes {
		t.Fatal("fixture does not exercise legal native capacity exceeding disk budget")
	}
	boundAffinitySnapshot(&snapshot)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxAffinityStateBytes-(2<<20) {
		t.Fatalf("checkpoint exceeds reserved byte budget: %d", len(encoded))
	}
	if !snapshot.Truncated || len(snapshot.Prefixes.Groups) == 0 {
		t.Fatal("capacity must yield useful partial checkpoint with loss signal")
	}
	last := snapshot.Prefixes.Groups[len(snapshot.Prefixes.Groups)-1]
	if last.AccessNumber != uint64(session.MerklePrefixSnapshotMaxGroups) {
		t.Fatal("newest hint omitted")
	}
	if snapshot.Prefixes.Groups[0].AccessNumber <= 1 {
		t.Fatal("oldest hint unexpectedly retained")
	}
}

func TestAffinityCheckpointInvalidMetadataFailsClosed(t *testing.T) {
	m, s := affinityTestManager(t, time.Hour, &FillFirstSelector{}, checkpointTestAuths())
	valid, err := m.SnapshotAffinity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*AffinitySnapshot){
		func(v *AffinitySnapshot) { v.Version++ },
		func(v *AffinitySnapshot) { v.CreatedAt = time.Now().Add(time.Minute) },
		func(v *AffinitySnapshot) { v.TTL = -time.Second },
		func(v *AffinitySnapshot) { v.Accounts = map[string]string{"a": "not-a-digest"} },
		func(v *AffinitySnapshot) { v.SubagentAffinity = !v.SubagentAffinity },
	} {
		candidate := valid
		mutate(&candidate)
		if err := m.RestoreAffinity(candidate, time.Now()); err == nil {
			t.Fatal("invalid metadata accepted")
		}
		if s.cache.Len() != 0 {
			t.Fatal("invalid checkpoint mutated bindings")
		}
	}
}
