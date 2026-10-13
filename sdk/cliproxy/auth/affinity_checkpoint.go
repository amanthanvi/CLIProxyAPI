package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const AffinitySnapshotVersion = 1

func (s *SessionAffinitySelector) quotaRebindEnabled() bool {
	if s == nil {
		return false
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.rebindOnQuotaOnly
}

func (s *SessionAffinitySelector) weightedFallback() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	_, weighted := s.fallback.(*WeightedRoundRobinSelector)
	return weighted
}

// AffinitySnapshot contains routing metadata, not credentials or prompt content.
// Session identifiers and hashes remain private operational metadata.
type AffinitySnapshot struct {
	Version          int                          `json:"version"`
	CreatedAt        time.Time                    `json:"created_at"`
	TTL              time.Duration                `json:"ttl"`
	SubagentAffinity bool                         `json:"subagent_affinity"`
	Bindings         []SessionBinding             `json:"bindings"`
	Prefixes         session.MerklePrefixSnapshot `json:"prefixes"`
	Accounts         map[string]string            `json:"accounts"`
	Truncated        bool                         `json:"truncated,omitempty"`
	ClaudeDates      *session.DatePinSnapshot     `json:"claude_dates,omitempty"`
}

// AffinityAccountIdentity hashes stable account/routing metadata, never tokens.
// Without account metadata, the existing eligible auth ID remains the identity.
func AffinityAccountIdentity(a *Auth) string {
	if a == nil {
		return ""
	}
	// Account fields survive OAuth refresh; token/version/generation fields do not.
	parts := []string{strings.ToLower(a.Provider), a.ID, a.Prefix}
	for _, key := range []string{"account_id", "organization_uuid", "account_uuid", "email"} {
		value, _ := a.Metadata[key].(string)
		if key == "account_uuid" && strings.TrimSpace(value) == "" {
			value, _ = a.Metadata["accountUuid"].(string)
		}
		parts = append(parts, key, strings.TrimSpace(value))
	}
	for _, key := range []string{"base_url", "provider"} {
		parts = append(parts, key, a.Attributes[key])
	}
	encoded, _ := json.Marshal(parts)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

// Caller holds stateMu exclusively; absence from a request's narrowed pool is
// not account removal and must not invalidate unrelated bindings.
func (s *SessionAffinitySelector) validateAccountHints(auths []*Auth) {
	if s.authIdentities == nil {
		s.authIdentities = make(map[string]string)
	}
	for _, a := range auths {
		if a == nil {
			continue
		}
		identity := AffinityAccountIdentity(a)
		if previous := s.authIdentities[a.ID]; previous != "" && previous != identity {
			s.cache.InvalidateAuth(a.ID)
			s.matcher.InvalidateAuth(a.ID)
		}
		s.authIdentities[a.ID] = identity
	}
}

func (s *SessionAffinitySelector) reconfigure(next *SessionAffinitySelector) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.fallback = next.fallback
	s.subagentAffinity = next.subagentAffinity
	s.rebindOnQuotaOnly = next.rebindOnQuotaOnly
	s.cache.mu.Lock()
	s.cache.ttl = next.cache.ttl
	cap := time.Now().Add(s.cache.ttl)
	for key, entry := range s.cache.groups {
		if entry.expiresAt.After(cap) {
			entry.expiresAt = cap
			s.cache.groups[key] = entry
			for _, alias := range entry.aliases {
				s.cache.entries[alias] = entry
			}
		}
	}
	s.cache.mu.Unlock()
	s.matcher.SetTTL(next.cache.ttl)
}

func (m *Manager) SnapshotAffinity(now time.Time) (AffinitySnapshot, error) {
	s, ok := m.Selector().(*SessionAffinitySelector)
	if !ok {
		return AffinitySnapshot{}, errors.New("native session affinity is disabled")
	}
	auths := m.List()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.validateAccountHints(auths)
	// Selection may have advanced between the caller's clock read and this lock.
	if current := time.Now(); current.After(now) {
		now = current
	}
	prefixes, err := s.matcher.Snapshot(now)
	if err != nil {
		return AffinitySnapshot{}, err
	}
	result := AffinitySnapshot{Version: AffinitySnapshotVersion, CreatedAt: now, TTL: s.cache.ttl, SubagentAffinity: s.subagentAffinity, Bindings: s.cache.Snapshot(now), Prefixes: prefixes, Accounts: make(map[string]string)}
	boundAffinitySnapshot(&result)
	needed := make(map[string]bool)
	for _, binding := range result.Bindings {
		needed[binding.AuthID] = true
	}
	for _, group := range result.Prefixes.Groups {
		needed[group.AuthID] = true
	}
	for _, a := range auths {
		if a != nil && !a.Disabled && needed[a.ID] {
			result.Accounts[a.ID] = AffinityAccountIdentity(a)
		}
	}
	return result, nil
}

// Reserve space for optional date pins and metadata. Count each group's account
// entry conservatively so high-capacity RAM caches still produce a useful file.
func boundAffinitySnapshot(snapshot *AffinitySnapshot) {
	budget := maxAffinityStateBytes - (2 << 20)
	bindings := snapshot.Bindings
	prefixes := snapshot.Prefixes.Groups
	keptBindings := make([]SessionBinding, 0, len(bindings))
	keptPrefixes := make([]session.MerklePrefixSnapshotGroup, 0, len(prefixes))
	keep := func(group any, authID string) bool {
		encoded, err := json.Marshal(group)
		accountKey, _ := json.Marshal(authID)
		size := len(encoded) + len(accountKey) + 70 + 2
		if err != nil || size > budget {
			snapshot.Truncated = true
			return false
		}
		budget -= size
		return true
	}
	for i, j := len(bindings)-1, len(prefixes)-1; i >= 0 || j >= 0; {
		if i >= 0 {
			if keep(bindings[i], bindings[i].AuthID) {
				keptBindings = append(keptBindings, bindings[i])
			}
			i--
		}
		if j >= 0 {
			if keep(prefixes[j], prefixes[j].AuthID) {
				keptPrefixes = append(keptPrefixes, prefixes[j])
			}
			j--
		}
	}
	for i, j := 0, len(keptBindings)-1; i < j; i, j = i+1, j-1 {
		keptBindings[i], keptBindings[j] = keptBindings[j], keptBindings[i]
	}
	for i, j := 0, len(keptPrefixes)-1; i < j; i, j = i+1, j-1 {
		keptPrefixes[i], keptPrefixes[j] = keptPrefixes[j], keptPrefixes[i]
	}
	snapshot.Bindings = keptBindings
	snapshot.Prefixes.Groups = keptPrefixes
}

func (m *Manager) RestoreAffinity(snapshot AffinitySnapshot, now time.Time) error {
	s, ok := m.Selector().(*SessionAffinitySelector)
	if !ok {
		return errors.New("native session affinity is disabled")
	}
	if snapshot.Version != AffinitySnapshotVersion || snapshot.CreatedAt.IsZero() || snapshot.CreatedAt.After(now.Add(5*time.Second)) || snapshot.TTL <= 0 || snapshot.TTL > 365*24*time.Hour {
		return errors.New("invalid affinity checkpoint metadata")
	}
	if len(snapshot.Accounts) > defaultMaxSessionEntries {
		return errors.New("affinity account capacity exceeded")
	}
	for id, digest := range snapshot.Accounts {
		if id == "" || len(id) > maxAffinityIdentifierBytes || len(digest) != 64 {
			return errors.New("invalid affinity account identity")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return errors.New("invalid affinity account identity")
		}
	}
	if err := validateSessionBindings(snapshot.Bindings, s.cache.maxEntries); err != nil {
		return err
	}
	for _, binding := range snapshot.Bindings {
		if binding.ExpiresAt.After(snapshot.CreatedAt.Add(snapshot.TTL)) {
			return errors.New("invalid affinity binding lifetime")
		}
	}
	auths := m.List()
	current := make(map[string]string, len(auths))
	for _, a := range auths {
		if a != nil && !a.Disabled {
			current[a.ID] = AffinityAccountIdentity(a)
		}
	}
	valid := func(id string) bool { return current[id] != "" && current[id] == snapshot.Accounts[id] }
	bindings := make([]SessionBinding, 0, len(snapshot.Bindings))
	for _, binding := range snapshot.Bindings {
		if valid(binding.AuthID) {
			bindings = append(bindings, binding)
		}
	}
	// Validate the full Merkle document before filtering stale account hints.
	if err := snapshot.Prefixes.Validate(now); err != nil {
		return err
	}
	if snapshot.ClaudeDates != nil {
		if err := snapshot.ClaudeDates.Validate(now); err != nil {
			return err
		}
	}
	prefixes := snapshot.Prefixes
	prefixes.Groups = nil
	for _, group := range snapshot.Prefixes.Groups {
		if valid(group.AuthID) {
			prefixes.Groups = append(prefixes.Groups, group)
		}
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if snapshot.SubagentAffinity != s.subagentAffinity {
		return errors.New("incompatible affinity checkpoint policy")
	}
	if err := s.matcher.Restore(prefixes, now); err != nil {
		return err
	}
	if err := s.cache.Restore(bindings, now); err != nil {
		return err
	}
	s.validateAccountHints(auths)
	return nil
}
