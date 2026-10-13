package session

import (
	"errors"
	"time"
)

const (
	DatePinSnapshotVersion = 1
	DatePinSnapshotMaxPins = 4096
	DatePinSnapshotTTL     = time.Hour
)

// DatePinSnapshot carries only opaque continuity-key digests and calendar pins.
// It does not carry previous response/request IDs, prompt IDs, or replay data.
type DatePinSnapshot struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Pins      []DatePin `json:"pins"`
}

type DatePin struct {
	Key          string    `json:"key"`
	Date         string    `json:"date"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccessNumber uint64    `json:"access_number"`
}

// Validate checks the full bounded snapshot before any cache is mutated.
func (snapshot DatePinSnapshot) Validate(now time.Time) error {
	if snapshot.Version != DatePinSnapshotVersion || !validCheckpointTime(now) || !validCheckpointTime(snapshot.CreatedAt) || snapshot.CreatedAt.After(now.Add(merkleCheckpointMaxClockSkew)) || len(snapshot.Pins) > DatePinSnapshotMaxPins {
		return errors.New("invalid date-pin checkpoint header")
	}
	seen := make(map[string]struct{})
	var previousAccess uint64
	for _, pin := range snapshot.Pins {
		date, err := time.Parse("2006-01-02", pin.Date)
		if !validCheckpointDigest(pin.Key) || err != nil || date.Year() < 1 || date.Format("2006-01-02") != pin.Date || pin.AccessNumber <= previousAccess || !validCheckpointTime(pin.ExpiresAt) || !pin.ExpiresAt.After(snapshot.CreatedAt) || pin.ExpiresAt.After(snapshot.CreatedAt.Add(DatePinSnapshotTTL+merkleCheckpointMaxClockSkew)) {
			return errors.New("invalid date-pin checkpoint record")
		}
		if _, duplicate := seen[pin.Key]; duplicate {
			return errors.New("duplicate date-pin checkpoint key")
		}
		seen[pin.Key] = struct{}{}
		previousAccess = pin.AccessNumber
	}
	return nil
}
