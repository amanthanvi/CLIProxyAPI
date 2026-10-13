package helps

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestClaudeDateCheckpointNativeRoundTrip(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	key, sequence, _, _, _ := BeginClaudeContinuity("synthetic-account", "synthetic-session", true, "")
	PinClaudeSessionDate(key, "", "2026-10-12", 0)
	CommitClaudeContinuity(key, sequence, "msg_private", "req_private", "11111111-2222-4333-8444-555555555555")
	now := time.Now()
	snapshot, err := SnapshotClaudeSessionDates(now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-account", "synthetic-session", "msg_private", "req_private", "11111111-2222-4333-8444-555555555555"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("checkpoint included raw identity or diagnostics")
		}
	}
	ResetClaudeDiagnosticsForTest()
	var decoded session.DatePinSnapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := RestoreClaudeSessionDates(decoded, now, 0); err != nil {
		t.Fatal(err)
	}
	if got := PinClaudeSessionDate(key, "", "2026-10-13", 0); got != "2026-10-12" {
		t.Fatal("restart changed pinned date")
	}
	claudeDiagnosticsState.Lock()
	entry := claudeDiagnosticsState.entries[key]
	claudeDiagnosticsState.Unlock()
	if !entry.expiresAt.Equal(snapshot.Pins[0].ExpiresAt) {
		t.Fatal("restore renewed absolute expiry")
	}
	if entry.previousMessageID != "" || entry.previousRequestID != "" || entry.promptID != "" || entry.committedSequence != 0 {
		t.Fatal("restored response diagnostics or replay state")
	}
	keyAgain, _, previousMessage, previousRequest, _ := BeginClaudeContinuity("synthetic-account", "synthetic-session", true, "")
	if keyAgain != key || previousMessage != "" || previousRequest != "" || PinClaudeSessionDate(keyAgain, "", "2026-10-13", 0) != "2026-10-12" {
		t.Fatal("native continuity did not adopt restored date-only pin")
	}
	changedKey, _, _, _, _ := BeginClaudeContinuity("replacement-account", "synthetic-session", true, "")
	if PinClaudeSessionDate(changedKey, "", "2026-10-13", 0) != "2026-10-13" {
		t.Fatal("replacement credential reused old pin")
	}
}

func TestClaudeDateCheckpointExpiryLivePrecedenceAndBounds(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	key := strings.Repeat("a", 64)
	snapshot := session.DatePinSnapshot{Version: session.DatePinSnapshotVersion, CreatedAt: now, Pins: []session.DatePin{{Key: key, Date: "2026-10-12", ExpiresAt: now.Add(time.Hour), AccessNumber: 1}}}
	if err := RestoreClaudeSessionDates(snapshot, now.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if len(claudeDiagnosticsState.entries) != 0 {
		t.Fatal("expired date pin restored")
	}
	if err := RestoreClaudeSessionDates(snapshot, now.Add(-time.Second), 0); err != nil {
		t.Fatal(err)
	}
	if !claudeDiagnosticsState.entries[key].expiresAt.Equal(now.Add(time.Hour - time.Second)) {
		t.Fatal("backward clock did not clamp native TTL")
	}
	claudeDiagnosticsState.Lock()
	entry := claudeDiagnosticsState.entries[key]
	entry.pinnedDate = "2026-10-13"
	claudeDiagnosticsState.entries[key] = entry
	claudeDiagnosticsState.Unlock()
	if err := RestoreClaudeSessionDates(snapshot, now, 0); err != nil {
		t.Fatal(err)
	}
	if PinClaudeSessionDate(key, "", "2026-10-14", 0) != "2026-10-13" {
		t.Fatal("stale snapshot replaced live date")
	}
	// Capacity includes live entries without a date, not merely exported date pins.
	claudeDiagnosticsState.Lock()
	for i := 0; i < claudeDiagnosticsMaxEntries-1; i++ {
		claudeDiagnosticsState.entries[fmt.Sprintf("live-%d", i)] = claudeDiagnosticsEntry{expiresAt: now.Add(time.Hour)}
	}
	claudeDiagnosticsState.Unlock()
	snapshot.Pins[0].Key = strings.Repeat("b", 64)
	if err := RestoreClaudeSessionDates(snapshot, now, 0); err != nil {
		t.Fatal(err)
	}
	if len(claudeDiagnosticsState.entries) != claudeDiagnosticsMaxEntries {
		t.Fatal("date restore exceeded native shared capacity")
	}
	if _, exists := claudeDiagnosticsState.entries[snapshot.Pins[0].Key]; exists {
		t.Fatal("date hint evicted live diagnostics")
	}
}

func TestClaudeDateCheckpointMalformedAtomic(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	key, _, _, _, _ := BeginClaudeContinuity("account", "session", true, "")
	PinClaudeSessionDate(key, "", "2026-10-12", 0)
	now := time.Now()
	valid, err := SnapshotClaudeSessionDates(now)
	if err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string]func(*session.DatePinSnapshot){
		"version": func(s *session.DatePinSnapshot) { s.Version++ },
		"clock":   func(s *session.DatePinSnapshot) { s.CreatedAt = now.Add(time.Minute) },
		"key":     func(s *session.DatePinSnapshot) { s.Pins[0].Key = "raw-session" },
		"date":    func(s *session.DatePinSnapshot) { s.Pins[0].Date = "2026-02-30" },
		"expiry":  func(s *session.DatePinSnapshot) { s.Pins[0].ExpiresAt = now.Add(2 * time.Hour) },
		"access":  func(s *session.DatePinSnapshot) { s.Pins[0].AccessNumber = 0 },
		"duplicate": func(s *session.DatePinSnapshot) {
			other := s.Pins[0]
			other.AccessNumber++
			s.Pins = append(s.Pins, other)
		},
		"capacity": func(s *session.DatePinSnapshot) { s.Pins = make([]session.DatePin, session.DatePinSnapshotMaxPins+1) },
	} {
		t.Run(name, func(t *testing.T) {
			encoded, _ := json.Marshal(valid)
			var malformed session.DatePinSnapshot
			if err := json.Unmarshal(encoded, &malformed); err != nil {
				t.Fatal(err)
			}
			corrupt(&malformed)
			before, _ := SnapshotClaudeSessionDates(now)
			if err := RestoreClaudeSessionDates(malformed, now, 0); err == nil {
				t.Fatal("malformed date snapshot accepted")
			}
			after, _ := SnapshotClaudeSessionDates(now)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("validation failure mutated live dates")
			}
		})
	}
}

func TestClaudeDateCheckpointConcurrentNativeRequests(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	var workers sync.WaitGroup
	for i := 0; i < 6; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for j := 0; j < 20; j++ {
				key, _, _, _, _ := BeginClaudeContinuity("account", fmt.Sprintf("session-%d-%d", index, j), true, "")
				PinClaudeSessionDate(key, "", "2026-10-13", 0)
				now := time.Now()
				snapshot, err := SnapshotClaudeSessionDates(now)
				if err != nil {
					t.Error(err)
					return
				}
				if err := RestoreClaudeSessionDates(snapshot, now, 0); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	workers.Wait()
}

const testSessionDateRetention = 7 * 24 * time.Hour

func TestClaudeSessionScopedDateCheckpointRoundTrip(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	key, _, _, _, _ := BeginClaudeContinuity("synthetic-account", "synthetic-session", true, "")
	PinClaudeSessionDate(key, "synthetic-session", "2026-10-12", testSessionDateRetention)
	now := time.Now()
	snapshot, err := SnapshotClaudeSessionDates(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pins) != 1 || snapshot.Pins[0].Key != ClaudeSessionDatePinKey("synthetic-session") {
		t.Fatalf("session-scoped pin not captured: %d pins", len(snapshot.Pins))
	}
	if snapshot.Pins[0].ExpiresAt.After(now.Add(session.DatePinSnapshotTTL)) {
		t.Fatal("session-scoped pin exceeded the checkpoint horizon")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-account", "synthetic-session"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("checkpoint included a raw identifier")
		}
	}
	ResetClaudeDiagnosticsForTest()
	if err := RestoreClaudeSessionDates(snapshot, now, testSessionDateRetention); err != nil {
		t.Fatal(err)
	}
	if len(claudeDiagnosticsState.entries) != 0 {
		t.Fatal("session-scoped pin restored as continuity state")
	}
	if got := LookupClaudeSessionDatePin("synthetic-session", testSessionDateRetention); got != "2026-10-12" {
		t.Fatalf("restored session pin = %q", got)
	}
	if !ClaudeSessionDatePinExpiryForTest("synthetic-session").Equal(snapshot.Pins[0].ExpiresAt) {
		t.Fatal("restore renewed absolute expiry")
	}
	// Session pins are credential independent, as upstream designed them.
	replacementKey, _, _, _, _ := BeginClaudeContinuity("replacement-account", "synthetic-session", true, "")
	if got := PinClaudeSessionDate(replacementKey, "synthetic-session", "2026-10-13", testSessionDateRetention); got != "2026-10-12" {
		t.Fatalf("resumed session re-anchored to %q", got)
	}
	if expiry := ClaudeSessionDatePinExpiryForTest("synthetic-session"); expiry.Before(time.Now().Add(testSessionDateRetention - time.Minute)) {
		t.Fatal("resumed session did not slide its pin back to the configured retention")
	}
}

func TestClaudeSessionScopedDateCheckpointPrecedenceAndCapacity(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	key := ClaudeSessionDatePinKey("synthetic-session")
	snapshot := session.DatePinSnapshot{Version: session.DatePinSnapshotVersion, CreatedAt: now, Pins: []session.DatePin{{Key: key, Date: "2026-10-12", ExpiresAt: now.Add(time.Hour), AccessNumber: 1}}}
	if err := RestoreClaudeSessionDates(snapshot, now.Add(time.Hour), testSessionDateRetention); err != nil {
		t.Fatal(err)
	}
	if len(claudeDiagnosticsState.datePins) != 0 {
		t.Fatal("expired session pin restored")
	}
	if err := RestoreClaudeSessionDates(snapshot, now.Add(-time.Second), testSessionDateRetention); err != nil {
		t.Fatal(err)
	}
	if !claudeDiagnosticsState.datePins[key].expiresAt.Equal(now.Add(time.Hour - time.Second)) {
		t.Fatal("backward clock did not clamp the restored horizon")
	}
	claudeDiagnosticsState.Lock()
	claudeDiagnosticsState.datePins[key] = claudeSessionDatePin{date: "2026-10-13", expiresAt: now.Add(48 * time.Hour)}
	claudeDiagnosticsState.Unlock()
	if err := RestoreClaudeSessionDates(snapshot, now, testSessionDateRetention); err != nil {
		t.Fatal(err)
	}
	if got := claudeDiagnosticsState.datePins[key]; got.date != "2026-10-13" || !got.expiresAt.Equal(now.Add(48*time.Hour)) {
		t.Fatal("stale snapshot replaced a live session pin")
	}
	claudeDiagnosticsState.Lock()
	for i := 0; i < claudeDiagnosticsMaxEntries-1; i++ {
		claudeDiagnosticsState.datePins[fmt.Sprintf("live-%d", i)] = claudeSessionDatePin{date: "2026-10-13", expiresAt: now.Add(time.Hour)}
	}
	claudeDiagnosticsState.Unlock()
	snapshot.Pins[0].Key = strings.Repeat("b", 64)
	if err := RestoreClaudeSessionDates(snapshot, now, testSessionDateRetention); err != nil {
		t.Fatal(err)
	}
	if len(claudeDiagnosticsState.datePins) != claudeDiagnosticsMaxEntries {
		t.Fatal("session pin restore exceeded shared capacity")
	}
	if _, exists := claudeDiagnosticsState.datePins[snapshot.Pins[0].Key]; exists {
		t.Fatal("session pin restore evicted live pins")
	}
}

func TestClaudeDateCheckpointBoundsCombinedPins(t *testing.T) {
	ResetClaudeDiagnosticsForTest()
	t.Cleanup(ResetClaudeDiagnosticsForTest)
	now := time.Now()
	claudeDiagnosticsState.Lock()
	for i := 0; i < claudeDiagnosticsMaxEntries; i++ {
		claudeDiagnosticsState.nextAccess++
		claudeDiagnosticsState.entries[fmt.Sprintf("%064x", i)] = claudeDiagnosticsEntry{pinnedDate: "2026-10-12", expiresAt: now.Add(time.Hour), lastAccess: claudeDiagnosticsState.nextAccess}
	}
	for i := 0; i < 3; i++ {
		claudeDiagnosticsState.datePins[ClaudeSessionDatePinKey(fmt.Sprintf("session-%d", i))] = claudeSessionDatePin{date: "2026-10-13", expiresAt: now.Add(time.Duration(i+1) * 24 * time.Hour)}
	}
	claudeDiagnosticsState.Unlock()
	snapshot, err := SnapshotClaudeSessionDates(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pins) != session.DatePinSnapshotMaxPins {
		t.Fatalf("snapshot pins = %d, want %d", len(snapshot.Pins), session.DatePinSnapshotMaxPins)
	}
	for i := 0; i < 3; i++ {
		if got := snapshot.Pins[len(snapshot.Pins)-3+i].Key; got != ClaudeSessionDatePinKey(fmt.Sprintf("session-%d", i)) {
			t.Fatal("session pins were not kept as the most recent records")
		}
	}
}
