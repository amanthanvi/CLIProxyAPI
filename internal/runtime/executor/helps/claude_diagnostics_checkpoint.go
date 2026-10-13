package helps

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// SnapshotClaudeSessionDates copies unexpired calendar pins without refreshing
// diagnostic lifetimes. The key is the native credential/session SHA-256 digest.
func SnapshotClaudeSessionDates(now time.Time) (session.DatePinSnapshot, error) {
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	snapshot := session.DatePinSnapshot{Version: session.DatePinSnapshotVersion, CreatedAt: now.UTC()}
	for key, entry := range claudeDiagnosticsState.entries {
		if entry.pinnedDate == "" || !now.Before(entry.expiresAt) {
			continue
		}
		snapshot.Pins = append(snapshot.Pins, session.DatePin{Key: key, Date: entry.pinnedDate, ExpiresAt: entry.expiresAt.UTC(), AccessNumber: entry.lastAccess})
	}
	sort.Slice(snapshot.Pins, func(i, j int) bool { return snapshot.Pins[i].AccessNumber < snapshot.Pins[j].AccessNumber })
	if err := snapshot.Validate(now); err != nil {
		return session.DatePinSnapshot{}, err
	}
	return snapshot, nil
}

// RestoreClaudeSessionDates restores date-only hints, not diagnostic continuity.
// Existing live entries win, expiration remains absolute, and the native capacity
// is shared with ordinary diagnostic entries rather than evicting live state.
func RestoreClaudeSessionDates(snapshot session.DatePinSnapshot, now time.Time) error {
	if err := snapshot.Validate(now); err != nil {
		return err
	}
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	if claudeDiagnosticsState.nextAccess > math.MaxUint64-uint64(len(snapshot.Pins))-1 || claudeDiagnosticsState.nextSequence > math.MaxUint64-uint64(len(snapshot.Pins))-1 {
		return errors.New("date-pin checkpoint generation exhausted")
	}
	remaining := claudeDiagnosticsMaxEntries
	for _, entry := range claudeDiagnosticsState.entries {
		if now.Before(entry.expiresAt) {
			remaining--
		}
	}
	var selected []int
	for i := len(snapshot.Pins) - 1; i >= 0 && remaining > 0; i-- {
		pin := snapshot.Pins[i]
		if !now.Before(pin.ExpiresAt) {
			continue
		}
		if entry, exists := claudeDiagnosticsState.entries[pin.Key]; exists && now.Before(entry.expiresAt) {
			continue
		}
		selected = append(selected, i)
		remaining--
	}
	for key, entry := range claudeDiagnosticsState.entries {
		if !now.Before(entry.expiresAt) {
			delete(claudeDiagnosticsState.entries, key)
		}
	}
	for i := len(selected) - 1; i >= 0; i-- {
		pin := snapshot.Pins[selected[i]]
		expiresAt := pin.ExpiresAt
		if limit := now.Add(claudeDiagnosticsTTL); expiresAt.After(limit) {
			expiresAt = limit
		}
		claudeDiagnosticsState.nextAccess++
		claudeDiagnosticsState.nextSequence++
		claudeDiagnosticsState.entries[pin.Key] = claudeDiagnosticsEntry{
			pinnedDate: pin.Date, expiresAt: expiresAt,
			lastAccess:      claudeDiagnosticsState.nextAccess,
			minimumSequence: claudeDiagnosticsState.nextSequence,
		}
	}
	return nil
}
