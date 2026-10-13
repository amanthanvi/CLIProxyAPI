package helps

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// SnapshotClaudeSessionDates copies unexpired calendar pins without refreshing
// diagnostic lifetimes. Continuity-scoped pins are keyed by the native
// credential/session SHA-256 digest; session-scoped pins (positive
// session-date-pin-days) are keyed by the session SHA-256 digest. Only one kind
// is populated for a given retention setting. Session-scoped expiries are capped
// to the checkpoint format's one-hour horizon; a resumed session slides its pin
// back to the configured retention on next use.
func SnapshotClaudeSessionDates(now time.Time) (session.DatePinSnapshot, error) {
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	snapshot := session.DatePinSnapshot{Version: session.DatePinSnapshotVersion, CreatedAt: now.UTC()}
	type orderedPin struct {
		pin   session.DatePin
		order uint64
		at    time.Time
	}
	var continuity, sessions []orderedPin
	for key, entry := range claudeDiagnosticsState.entries {
		if entry.pinnedDate == "" || !now.Before(entry.expiresAt) {
			continue
		}
		continuity = append(continuity, orderedPin{pin: session.DatePin{Key: key, Date: entry.pinnedDate, ExpiresAt: entry.expiresAt.UTC()}, order: entry.lastAccess})
	}
	horizon := now.Add(session.DatePinSnapshotTTL)
	for key, pin := range claudeDiagnosticsState.datePins {
		if pin.date == "" || !now.Before(pin.expiresAt) {
			continue
		}
		expiresAt := pin.expiresAt
		if expiresAt.After(horizon) {
			expiresAt = horizon
		}
		sessions = append(sessions, orderedPin{pin: session.DatePin{Key: key, Date: pin.date, ExpiresAt: expiresAt.UTC()}, at: pin.expiresAt})
	}
	sort.Slice(continuity, func(i, j int) bool { return continuity[i].order < continuity[j].order })
	// Sliding expiry makes the latest expiry the most recently used session pin.
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].at.Equal(sessions[j].at) {
			return sessions[i].at.Before(sessions[j].at)
		}
		return sessions[i].pin.Key < sessions[j].pin.Key
	})
	ordered := append(continuity, sessions...)
	if extra := len(ordered) - session.DatePinSnapshotMaxPins; extra > 0 {
		ordered = ordered[extra:]
	}
	for i, item := range ordered {
		item.pin.AccessNumber = uint64(i + 1)
		snapshot.Pins = append(snapshot.Pins, item.pin)
	}
	if err := snapshot.Validate(now); err != nil {
		return session.DatePinSnapshot{}, err
	}
	return snapshot, nil
}

// RestoreClaudeSessionDates restores date-only hints, not diagnostic continuity.
// Existing live entries win, expiration remains absolute, and the native capacity
// is shared with live state rather than evicting it. A positive retention
// restores into the session-scoped pin store used by session-date-pin-days;
// otherwise pins restore as continuity-scoped entries. A pin of the other kind
// never matches a lookup key and expires within the checkpoint horizon.
func RestoreClaudeSessionDates(snapshot session.DatePinSnapshot, now time.Time, retention time.Duration) error {
	if err := snapshot.Validate(now); err != nil {
		return err
	}
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	if retention > 0 {
		restoreClaudeSessionScopedDatesLocked(snapshot, now, retention)
		return nil
	}
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

func restoreClaudeSessionScopedDatesLocked(snapshot session.DatePinSnapshot, now time.Time, retention time.Duration) {
	remaining := claudeDiagnosticsMaxEntries
	for _, pin := range claudeDiagnosticsState.datePins {
		if !now.After(pin.expiresAt) {
			remaining--
		}
	}
	var selected []int
	for i := len(snapshot.Pins) - 1; i >= 0 && remaining > 0; i-- {
		pin := snapshot.Pins[i]
		if !now.Before(pin.ExpiresAt) {
			continue
		}
		if live, exists := claudeDiagnosticsState.datePins[pin.Key]; exists && !now.After(live.expiresAt) {
			continue
		}
		selected = append(selected, i)
		remaining--
	}
	for key, pin := range claudeDiagnosticsState.datePins {
		if now.After(pin.expiresAt) {
			delete(claudeDiagnosticsState.datePins, key)
		}
	}
	limit := now.Add(min(retention, session.DatePinSnapshotTTL))
	for i := len(selected) - 1; i >= 0; i-- {
		pin := snapshot.Pins[selected[i]]
		expiresAt := pin.ExpiresAt
		if expiresAt.After(limit) {
			expiresAt = limit
		}
		claudeDiagnosticsState.datePins[pin.Key] = claudeSessionDatePin{date: pin.Date, expiresAt: expiresAt}
	}
}
