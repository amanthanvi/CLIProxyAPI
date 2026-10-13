package session

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func checkpointMatcher(now time.Time, ttl time.Duration) *MerklePrefixMatcher {
	return NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{TTL: ttl, NowFunc: func() time.Time { return now }})
}

func roundTripMerkle(t *testing.T, matcher *MerklePrefixMatcher, now time.Time) *MerklePrefixMatcher {
	t.Helper()
	snapshot, err := matcher.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-turn") {
		t.Fatal("checkpoint contains raw conversation text")
	}
	var decoded MerklePrefixSnapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored := checkpointMatcher(now, matcher.ttl)
	if err := restored.Restore(decoded, now); err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestMerkleCheckpointGrowingHistoryAndFork(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	initial := turnsFromTexts("private-turn-A", "private-turn-B", "private-turn-C")
	initialResult := source.BindWithResult("provider:model:caller", initial, "auth-A")
	growing := turnsFromTexts("private-turn-A", "private-turn-B", "private-turn-C", "private-turn-D")
	fork := turnsFromTexts("private-turn-A", "private-turn-B", "private-turn-fork")
	restored := roundTripMerkle(t, source, now)
	match, ok := restored.Match("provider:model:caller", growing)
	if !ok || match.AuthID != "auth-A" || match.SessionID != initialResult.SessionID || match.IsFork {
		t.Fatalf("growing history did not retain native identity: %#v", match)
	}
	wantFork, ok := source.Match("provider:model:caller", fork)
	gotFork, gotOK := restored.Match("provider:model:caller", fork)
	if !ok || !gotOK || !gotFork.IsFork || gotFork.SessionID != wantFork.SessionID || gotFork.ParentSessionID != wantFork.ParentSessionID {
		t.Fatalf("fork topology changed: got %#v, want %#v", gotFork, wantFork)
	}
	source.BindWithResult("provider:model:caller", fork, "auth-fork")
	source.BindWithResult("provider:model:caller", append(fork, turnsFromTexts("private-turn-fork-growing")...), "auth-fork")
	restoredFork := roundTripMerkle(t, source, now)
	match, ok = restoredFork.Match("provider:model:caller", fork)
	if !ok || match.AuthID != "auth-fork" {
		t.Fatalf("restored fork binding missing: %#v", match)
	}
	for _, namespace := range []string{"other-provider:model:caller", "provider:other-model:caller", "provider:model:other-caller"} {
		if _, ok := restoredFork.Match(namespace, initial); ok {
			t.Fatalf("checkpoint leaked across namespace %q", namespace)
		}
	}
}

func TestMerkleCheckpointCompactionIndexesAndEnvironment(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	initial := turnsFromTexts("private-turn-1", "private-turn-2", "private-turn-3", "private-turn-4", "private-turn-5")
	initial = append([]CanonicalTurn{{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "private-turn-environment"}}}}, initial...)
	root := source.BindWithResult("namespace", initial, "auth-A")
	compacted := append(initial[:1:1], turnsFromTexts("private-turn-summary", "private-turn-4", "private-turn-5", "private-turn-6")...)
	restored := roundTripMerkle(t, source, now)
	match, ok := restored.Match("namespace", compacted)
	if !ok || !match.IsCompaction || match.AuthID != "auth-A" || match.ParentSessionID != root.SessionID {
		t.Fatalf("restored compaction tail index failed: %#v", match)
	}
	compaction := source.BindWithResult("namespace", compacted, "auth-A")
	restored = roundTripMerkle(t, source, now)
	match, ok = restored.Match("namespace", append(compacted, turnsFromTexts("private-turn-7")...))
	if !ok || match.SessionID != compaction.SessionID || match.ParentSessionID != compaction.ParentSessionID {
		t.Fatalf("compaction lineage changed: %#v", match)
	}
	changed := append([]CanonicalTurn(nil), compacted...)
	changed[0] = CanonicalTurn{Role: "system", Parts: []CanonicalPart{{Kind: "text", Value: "different-environment"}}}
	if _, ok := restored.Match("namespace", changed); ok {
		t.Fatal("compaction matched a different environment")
	}
}

func TestMerkleCheckpointExpiryAndTTL(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	source.Bind("namespace", turnsFromTexts("private-turn"), "auth-A")
	snapshot, err := source.Snapshot(now.Add(10 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		at    time.Time
		ttl   time.Duration
		want  time.Time
		count int
	}{
		{"preserve", now.Add(20 * time.Minute), 2 * time.Hour, now.Add(time.Hour), 1},
		{"clamp", now.Add(20 * time.Minute), 5 * time.Minute, now.Add(25 * time.Minute), 1},
		{"expired", now.Add(time.Hour), 2 * time.Hour, time.Time{}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := checkpointMatcher(test.at, test.ttl)
			if err := destination.Restore(snapshot, test.at); err != nil {
				t.Fatal(err)
			}
			got, err := destination.Snapshot(test.at)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Groups) != test.count {
				t.Fatalf("restored groups = %d", len(got.Groups))
			}
			if test.count > 0 && !got.Groups[0].ExpiresAt.Equal(test.want) {
				t.Fatalf("expiry = %v, want %v", got.Groups[0].ExpiresAt, test.want)
			}
		})
	}
	source.SetTTL(5 * time.Minute)
	got, err := source.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Groups[0].ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatal("SetTTL did not clamp")
	}
	source.SetTTL(2 * time.Hour)
	got, err = source.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Groups[0].ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatal("SetTTL increase refreshed lifetime")
	}
}

func TestMerkleCheckpointMalformedAtomicAndDetached(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	source.Bind("namespace", turnsFromTexts("private-turn-A"), "auth-A")
	source.Bind("namespace", turnsFromTexts("private-turn-B"), "auth-B")
	valid, err := source.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*MerklePrefixSnapshot){
		"version":    func(s *MerklePrefixSnapshot) { s.Version++ },
		"future":     func(s *MerklePrefixSnapshot) { s.CreatedAt = now.Add(time.Minute) },
		"zero clock": func(s *MerklePrefixSnapshot) { s.CreatedAt = time.Time{} },
		"ttl":        func(s *MerklePrefixSnapshot) { s.TTL = 0 },
		"expiry":     func(s *MerklePrefixSnapshot) { s.Groups[0].ExpiresAt = s.CreatedAt.Add(s.TTL + time.Second) },
		"old expiry": func(s *MerklePrefixSnapshot) { s.Groups[0].ExpiresAt = s.CreatedAt },
		"digest":     func(s *MerklePrefixSnapshot) { s.Groups[1].Fingerprints[0] = "raw-turn" },
		"namespace": func(s *MerklePrefixSnapshot) {
			s.Groups[0].Namespace = strings.Repeat("x", MerklePrefixSnapshotMaxNamespaceBytes+1)
		},
		"auth": func(s *MerklePrefixSnapshot) {
			s.Groups[0].AuthID = strings.Repeat("x", MerklePrefixSnapshotMaxAuthIDBytes+1)
		},
		"session": func(s *MerklePrefixSnapshot) { s.Groups[0].SessionID = "arbitrary" },
		"prefix":  func(s *MerklePrefixSnapshot) { s.Groups[0].MinPrefixLength = 2 },
		"tail": func(s *MerklePrefixSnapshot) {
			s.Groups[0].TailFingerprints = make([]string, maxCompactionProbeWindow+1)
		},
		"duplicate": func(s *MerklePrefixSnapshot) {
			s.Groups[1].Fingerprints = append([]string(nil), s.Groups[0].Fingerprints...)
		},
		"access order":    func(s *MerklePrefixSnapshot) { s.Groups[1].AccessNumber = s.Groups[0].AccessNumber },
		"access overflow": func(s *MerklePrefixSnapshot) { s.AccessCounter = math.MaxUint64 },
		"cycle": func(s *MerklePrefixSnapshot) {
			s.Groups[0].ParentSessionID = s.Groups[1].SessionID
			s.Groups[1].ParentSessionID = s.Groups[0].SessionID
		},
		"groups": func(s *MerklePrefixSnapshot) {
			s.Groups = make([]MerklePrefixSnapshotGroup, MerklePrefixSnapshotMaxGroups+1)
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			encoded, _ := json.Marshal(valid)
			var malformed MerklePrefixSnapshot
			if err := json.Unmarshal(encoded, &malformed); err != nil {
				t.Fatal(err)
			}
			corrupt(&malformed)
			destination := checkpointMatcher(now, time.Hour)
			destination.Bind("live", turnsFromTexts("private-turn-live"), "auth-live")
			before, _ := destination.Snapshot(now)
			if err := destination.Restore(malformed, now); err == nil {
				t.Fatal("malformed checkpoint accepted")
			}
			after, _ := destination.Snapshot(now)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed validation mutated live matcher")
			}
		})
	}
	valid.Groups[0].Fingerprints[0] = strings.Repeat("0", 64)
	if _, ok := source.Match("namespace", turnsFromTexts("private-turn-A")); !ok {
		t.Fatal("snapshot aliases matcher storage")
	}
	valid, _ = source.Snapshot(now)
	destination := checkpointMatcher(now, time.Hour)
	if err := destination.Restore(valid, now); err != nil {
		t.Fatal(err)
	}
	valid.Groups[0].Fingerprints[0] = strings.Repeat("0", 64)
	if _, ok := destination.Match("namespace", turnsFromTexts("private-turn-A")); !ok {
		t.Fatal("restore aliases snapshot storage")
	}
}

func TestMerkleCheckpointBoundsAndLivePrecedence(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	for i := 0; i < 5; i++ {
		source.Bind("namespace", turnsFromTexts(fmt.Sprintf("private-turn-%d", i)), fmt.Sprintf("auth-%d", i))
	}
	snapshot, _ := source.Snapshot(now)
	destination := NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{TTL: time.Hour, MaxTurns: 1, MaxGroups: 2, MaxPrefixes: 2, NowFunc: func() time.Time { return now }})
	destination.Bind("namespace", turnsFromTexts("private-turn-4"), "auth-live")
	if err := destination.Restore(snapshot, now); err != nil {
		t.Fatal(err)
	}
	got, err := destination.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("group bound not respected: %d", len(got.Groups))
	}
	match, ok := destination.Match("namespace", turnsFromTexts("private-turn-4"))
	if !ok || match.AuthID != "auth-live" {
		t.Fatal("checkpoint replaced live binding")
	}
	match, ok = destination.Match("namespace", turnsFromTexts("private-turn-3"))
	if !ok || match.AuthID != "auth-3" {
		t.Fatal("checkpoint failed to retain newest fitting hint")
	}
	if _, ok := destination.Match("namespace", turnsFromTexts("private-turn-0")); ok {
		t.Fatal("old checkpoint exceeded capacity")
	}
	// A stale longer trajectory with the same session cannot mask a newer live rebind.
	source = checkpointMatcher(now, time.Hour)
	source.Bind("namespace", turnsFromTexts("private-turn-A", "private-turn-B"), "auth-old")
	snapshot, _ = source.Snapshot(now)
	destination = checkpointMatcher(now, time.Hour)
	destination.Bind("namespace", turnsFromTexts("private-turn-A"), "auth-new")
	if err := destination.Restore(snapshot, now); err != nil {
		t.Fatal(err)
	}
	match, ok = destination.Match("namespace", turnsFromTexts("private-turn-A", "private-turn-B", "private-turn-C"))
	if !ok || match.AuthID != "auth-new" {
		t.Fatal("stale extension masked newer live session")
	}
}

func TestMerkleCheckpointConcurrentNativeOperations(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	matcher := checkpointMatcher(now, time.Hour)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for j := 0; j < 30; j++ {
				turns := turnsFromTexts(fmt.Sprintf("private-turn-%d-%d", index, j))
				matcher.Bind("namespace", turns, "auth")
				matcher.Match("namespace", turns)
				matcher.Touch("namespace", turns, "auth")
				snapshot, err := matcher.Snapshot(now)
				if err != nil {
					t.Error(err)
					return
				}
				if err := matcher.Restore(snapshot, now); err != nil {
					t.Error(err)
					return
				}
				matcher.SetTTL(time.Hour)
			}
		}(i)
	}
	workers.Wait()
}

func TestMerkleCheckpointHardBoundsAndClockSkew(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	source := checkpointMatcher(now, time.Hour)
	source.Bind("namespace", turnsFromTexts("private-turn-A"), "auth-A")
	snapshot, err := source.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	destination := checkpointMatcher(now.Add(-time.Second), time.Hour)
	if err := destination.Restore(snapshot, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := destination.Snapshot(now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Groups[0].ExpiresAt.Equal(now.Add(time.Hour - time.Second)) {
		t.Fatal("clock skew extended restored TTL")
	}
	tooManyTurns := snapshot
	tooManyTurns.Groups = append([]MerklePrefixSnapshotGroup(nil), snapshot.Groups...)
	tooManyTurns.Groups[0].Fingerprints = make([]string, MerklePrefixSnapshotMaxTurns+1)
	if err := tooManyTurns.Validate(now); err == nil {
		t.Fatal("oversized trajectory accepted")
	}
	tooManyPrefixes := snapshot
	tooManyPrefixes.Groups = nil
	for i := 0; i <= MerklePrefixSnapshotMaxPrefixes/MerklePrefixSnapshotMaxTurns; i++ {
		group := snapshot.Groups[0]
		group.Namespace = fmt.Sprintf("namespace-%d", i)
		group.AccessNumber = uint64(i + 1)
		group.Fingerprints = make([]string, MerklePrefixSnapshotMaxTurns)
		for j := range group.Fingerprints {
			group.Fingerprints[j] = snapshot.Groups[0].Fingerprints[0]
		}
		tooManyPrefixes.Groups = append(tooManyPrefixes.Groups, group)
	}
	tooManyPrefixes.AccessCounter = uint64(len(tooManyPrefixes.Groups))
	if err := tooManyPrefixes.Validate(now); err == nil {
		t.Fatal("oversized global prefix index accepted")
	}
	// Empty-startup restoration preserves individual access ranks and advances the
	// next native generation beyond every saved rank.
	destination = checkpointMatcher(now, time.Hour)
	if err := destination.Restore(snapshot, now); err != nil {
		t.Fatal(err)
	}
	got, err = destination.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Groups, got.Groups) {
		t.Fatal("restored access order or topology changed")
	}
	result := destination.BindWithResult("namespace", turnsFromTexts("private-turn-B"), "auth-B")
	if result.AccessNumber <= snapshot.AccessCounter {
		t.Fatal("native generation regressed after restore")
	}
}
