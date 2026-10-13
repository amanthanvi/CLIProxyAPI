package session

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	MerklePrefixSnapshotVersion           = 1
	MerklePrefixSnapshotMaxGroups         = defaultMatcherMaxGroups
	MerklePrefixSnapshotMaxPrefixes       = defaultMatcherMaxPrefixes
	MerklePrefixSnapshotMaxTurns          = maxCanonicalTurns
	MerklePrefixSnapshotMaxNamespaceBytes = 4096
	MerklePrefixSnapshotMaxAuthIDBytes    = 4096
	merkleCheckpointMaxClockSkew          = 5 * time.Second
)

// MerklePrefixSnapshot contains bounded affinity hints, never canonical turns or payloads.
// Groups are ordered by access number from oldest to newest. These opaque identifiers
// and content hashes remain sensitive correlation metadata.
type MerklePrefixSnapshot struct {
	Version       int                         `json:"version"`
	CreatedAt     time.Time                   `json:"created_at"`
	TTL           time.Duration               `json:"ttl_ns"`
	AccessCounter uint64                      `json:"access_counter"`
	Groups        []MerklePrefixSnapshotGroup `json:"groups"`
}

type MerklePrefixSnapshotGroup struct {
	Namespace         string    `json:"namespace"`
	AuthID            string    `json:"auth_id"`
	SessionID         string    `json:"session_id"`
	ParentSessionID   string    `json:"parent_session_id,omitempty"`
	MinPrefixLength   int       `json:"min_prefix_length"`
	IsFork            bool      `json:"is_fork,omitempty"`
	IsCompaction      bool      `json:"is_compaction,omitempty"`
	NodeKind          string    `json:"node_kind,omitempty"`
	EnvironmentDigest string    `json:"environment_digest,omitempty"`
	Fingerprints      []string  `json:"fingerprints"`
	TailFingerprints  []string  `json:"tail_fingerprints,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	AccessNumber      uint64    `json:"access_number"`
}

// Validate checks the complete bounded schema before a caller filters records.
func (snapshot MerklePrefixSnapshot) Validate(now time.Time) error {
	return validateMerklePrefixSnapshot(snapshot, now)
}

// SetTTL retains native topology across compatible selector reconfiguration.
// A shorter TTL clamps live expirations; a longer one does not refresh them.
func (m *MerklePrefixMatcher) SetTTL(ttl time.Duration) {
	if m == nil || ttl <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.ttl = ttl
	limit := now.Add(ttl)
	for _, ns := range m.groups {
		for _, group := range ns.groups {
			if group.expiresAt.After(limit) {
				group.expiresAt = limit
			}
		}
	}
	m.cleanupLocked(now)
}

// Snapshot copies unexpired groups without refreshing their lifetime or access order.
// Oversized custom matcher capacities are reduced to the checkpoint's hard bounds.
func (m *MerklePrefixMatcher) Snapshot(now time.Time) (MerklePrefixSnapshot, error) {
	if m == nil || !validCheckpointTime(now) {
		return MerklePrefixSnapshot{}, errors.New("invalid Merkle checkpoint clock or matcher")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := MerklePrefixSnapshot{Version: MerklePrefixSnapshotVersion, CreatedAt: now.UTC(), TTL: m.ttl, AccessCounter: m.accessCounter}
	prefixes := 0
	if m.lru != nil {
		for element := m.lru.Back(); element != nil; element = element.Prev() {
			group, ok := element.Value.(*lcpGroup)
			if !ok || group == nil || !now.Before(group.expiresAt) {
				continue
			}
			if len(snapshot.Groups) >= MerklePrefixSnapshotMaxGroups || len(group.fingerprints) > MerklePrefixSnapshotMaxTurns || prefixes+len(group.fingerprints) > MerklePrefixSnapshotMaxPrefixes {
				continue
			}
			snapshot.Groups = append(snapshot.Groups, MerklePrefixSnapshotGroup{
				Namespace: group.namespace, AuthID: group.authID, SessionID: group.sessionID,
				ParentSessionID: group.parentSessionID, MinPrefixLength: group.minPrefixLength,
				IsFork: group.isFork, IsCompaction: group.isCompaction, NodeKind: group.nodeKind,
				EnvironmentDigest: group.environmentDigest,
				Fingerprints:      append([]string(nil), group.fingerprints...),
				TailFingerprints:  append([]string(nil), group.tailFingerprints...),
				ExpiresAt:         group.expiresAt.UTC(), AccessNumber: group.lastAccessNumber,
			})
			prefixes += len(group.fingerprints)
		}
	}
	for i, j := 0, len(snapshot.Groups)-1; i < j; i, j = i+1, j-1 {
		snapshot.Groups[i], snapshot.Groups[j] = snapshot.Groups[j], snapshot.Groups[i]
	}
	sort.Slice(snapshot.Groups, func(i, j int) bool { return snapshot.Groups[i].AccessNumber < snapshot.Groups[j].AccessNumber })
	if err := validateMerklePrefixSnapshot(snapshot, now); err != nil {
		return MerklePrefixSnapshot{}, err
	}
	return snapshot, nil
}

// Restore validates all records before mutation and rebuilds the native indexes.
// Expired hints are discarded, expiry is capped by the receiving TTL, and current
// live groups are never replaced or evicted to make room for restored hints.
func (m *MerklePrefixMatcher) Restore(snapshot MerklePrefixSnapshot, now time.Time) error {
	if m == nil {
		return errors.New("invalid Merkle checkpoint matcher")
	}
	if err := validateMerklePrefixSnapshot(snapshot, now); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reserve enough generations for index construction without wrapping an existing
	// counter. All validation, including this state-dependent check, precedes mutation.
	baseCounter := max(m.accessCounter, snapshot.AccessCounter)
	if baseCounter > math.MaxUint64-uint64(len(snapshot.Groups))-1 {
		return errors.New("Merkle checkpoint access counter exhausted")
	}
	remainingGroups, remainingPrefixes := m.maxGroups, m.maxPrefixes
	liveSessions := make(map[string]map[string]struct{})
	for namespace, ns := range m.groups {
		for _, group := range ns.groups {
			if now.Before(group.expiresAt) {
				remainingGroups--
				remainingPrefixes -= len(group.prefixKeys)
				if liveSessions[namespace] == nil {
					liveSessions[namespace] = make(map[string]struct{})
				}
				liveSessions[namespace][group.sessionID] = struct{}{}
			}
		}
	}
	var selected []int
	for i := len(snapshot.Groups) - 1; i >= 0; i-- {
		group := snapshot.Groups[i]
		if !now.Before(group.ExpiresAt) || len(group.Fingerprints) > m.maxTurns || remainingGroups <= 0 || len(group.Fingerprints) > remainingPrefixes {
			continue
		}
		if _, exists := liveSessions[group.Namespace][group.SessionID]; exists {
			continue
		}
		if ns := m.groups[group.Namespace]; ns != nil {
			if existing := ns.groups[sequenceKey(group.Fingerprints)]; existing != nil && now.Before(existing.expiresAt) {
				continue
			}
		}
		selected = append(selected, i)
		remainingGroups--
		remainingPrefixes -= len(group.Fingerprints)
	}
	m.prepareLocked()
	m.cleanupLocked(now)
	// Insert hints before current live entries in eviction order. addGroupLocked
	// supplies the normal native prefix and compaction-tail indexes.
	firstLive := m.lru.Front()
	m.accessCounter = baseCounter
	for i := len(selected) - 1; i >= 0; i-- {
		record := snapshot.Groups[selected[i]]
		expiresAt := record.ExpiresAt
		if limit := now.Add(m.ttl); expiresAt.After(limit) {
			expiresAt = limit
		}
		group := &lcpGroup{
			key: sequenceKey(record.Fingerprints), namespace: record.Namespace,
			authID: record.AuthID, sessionID: record.SessionID, parentSessionID: record.ParentSessionID,
			minPrefixLength: record.MinPrefixLength, isFork: record.IsFork,
			isCompaction: record.IsCompaction, nodeKind: record.NodeKind,
			environmentDigest: record.EnvironmentDigest,
			fingerprints:      append([]string(nil), record.Fingerprints...),
			tailFingerprints:  append([]string(nil), record.TailFingerprints...), expiresAt: expiresAt,
		}
		m.addGroupLocked(group)
		if firstLive == nil {
			group.lastAccessNumber = record.AccessNumber
		}
		if firstLive != nil {
			m.lru.MoveBefore(m.lruElements[group], firstLive)
		}
	}
	return nil
}

func validateMerklePrefixSnapshot(snapshot MerklePrefixSnapshot, now time.Time) error {
	if snapshot.Version != MerklePrefixSnapshotVersion || !validCheckpointTime(now) || !validCheckpointTime(snapshot.CreatedAt) || snapshot.CreatedAt.After(now.Add(merkleCheckpointMaxClockSkew)) || snapshot.TTL <= 0 || snapshot.AccessCounter == math.MaxUint64 || len(snapshot.Groups) > MerklePrefixSnapshotMaxGroups {
		return errors.New("invalid Merkle checkpoint header")
	}
	seen := make(map[string]map[string]struct{})
	parents := make(map[string]map[string]string)
	prefixCount := 0
	var previousAccess uint64
	for _, group := range snapshot.Groups {
		if strings.TrimSpace(group.Namespace) == "" || len(group.Namespace) > MerklePrefixSnapshotMaxNamespaceBytes || strings.TrimSpace(group.AuthID) == "" || len(group.AuthID) > MerklePrefixSnapshotMaxAuthIDBytes || !validLCPSessionID(group.SessionID) || (group.ParentSessionID != "" && (!validLCPSessionID(group.ParentSessionID) || group.ParentSessionID == group.SessionID)) {
			return errors.New("invalid Merkle checkpoint identifiers")
		}
		if len(group.Fingerprints) == 0 || len(group.Fingerprints) > MerklePrefixSnapshotMaxTurns || group.MinPrefixLength <= 0 || group.MinPrefixLength > len(group.Fingerprints) || len(group.TailFingerprints) > maxCompactionProbeWindow || (group.EnvironmentDigest != "" && !validCheckpointDigest(group.EnvironmentDigest)) {
			return errors.New("invalid Merkle checkpoint fingerprint bounds")
		}
		prefixCount += len(group.Fingerprints)
		if prefixCount > MerklePrefixSnapshotMaxPrefixes {
			return errors.New("Merkle checkpoint prefix capacity exceeded")
		}
		for _, values := range [][]string{group.Fingerprints, group.TailFingerprints} {
			for _, value := range values {
				if !validCheckpointDigest(value) {
					return errors.New("invalid Merkle checkpoint digest")
				}
			}
		}
		if (group.IsFork && group.IsCompaction) || (group.NodeKind != "" && group.NodeKind != "fork" && group.NodeKind != "compaction") || (group.IsFork && group.NodeKind != "fork") || (group.IsCompaction && group.NodeKind != "compaction") || ((group.IsFork || group.IsCompaction) && group.ParentSessionID == "") {
			return errors.New("invalid Merkle checkpoint topology")
		}
		if !validCheckpointTime(group.ExpiresAt) || !group.ExpiresAt.After(snapshot.CreatedAt) || group.ExpiresAt.After(snapshot.CreatedAt.Add(snapshot.TTL)) || group.AccessNumber <= previousAccess || group.AccessNumber > snapshot.AccessCounter {
			return errors.New("invalid Merkle checkpoint expiry or access order")
		}
		previousAccess = group.AccessNumber
		key := sequenceKey(group.Fingerprints)
		if seen[group.Namespace] == nil {
			seen[group.Namespace] = make(map[string]struct{})
		}
		if _, duplicate := seen[group.Namespace][key]; duplicate {
			return errors.New("duplicate Merkle checkpoint sequence")
		}
		seen[group.Namespace][key] = struct{}{}
		if parents[group.Namespace] == nil {
			parents[group.Namespace] = make(map[string]string)
		}
		if parent, exists := parents[group.Namespace][group.SessionID]; exists && parent != group.ParentSessionID {
			return errors.New("inconsistent Merkle checkpoint parent")
		}
		parents[group.Namespace][group.SessionID] = group.ParentSessionID
	}
	for _, namespace := range parents {
		checked := make(map[string]bool)
		for sessionID := range namespace {
			path := make(map[string]struct{})
			for current := sessionID; current != "" && !checked[current]; current = namespace[current] {
				if _, cycle := path[current]; cycle {
					return errors.New("cyclic Merkle checkpoint topology")
				}
				path[current] = struct{}{}
			}
			for current := range path {
				checked[current] = true
			}
		}
	}
	return nil
}

func validCheckpointTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 1 && value.Year() <= 9999
}

func validCheckpointDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validLCPSessionID(value string) bool {
	return strings.HasPrefix(value, "lcp:v1:") && validCheckpointDigest(strings.TrimPrefix(value, "lcp:v1:"))
}
