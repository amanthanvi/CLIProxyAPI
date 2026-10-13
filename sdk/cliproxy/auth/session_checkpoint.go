package auth

import (
	"errors"
	"time"
)

const maxAffinityIdentifierBytes = 4096

// SessionBinding is one atomic alias group. ExpiresAt is never renewed on restore.
type SessionBinding struct {
	AuthID    string    `json:"auth_id"`
	Aliases   []string  `json:"aliases"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *SessionCache) Snapshot(now time.Time) []SessionBinding {
	c.mu.RLock()
	defer c.mu.RUnlock()
	groups := make([]SessionBinding, 0, len(c.groups))
	for elem := c.evictionOrder.Front(); elem != nil; elem = elem.Next() {
		entry := c.groups[elem.Value.(string)]
		if now.Before(entry.expiresAt) {
			groups = append(groups, SessionBinding{entry.authID, append([]string(nil), entry.aliases...), entry.expiresAt})
		}
	}
	return groups
}

func validateSessionBindings(groups []SessionBinding, maxEntries int) error {
	if len(groups) > maxEntries {
		return errors.New("affinity binding capacity exceeded")
	}
	seen := make(map[string]bool)
	for _, group := range groups {
		if group.AuthID == "" || len(group.AuthID) > maxAffinityIdentifierBytes || group.ExpiresAt.IsZero() || len(group.Aliases) == 0 || len(group.Aliases) > maxStableSessionAliases+1 {
			return errors.New("invalid affinity binding")
		}
		for _, alias := range group.Aliases {
			if alias == "" || len(alias) > maxAffinityIdentifierBytes || seen[alias] {
				return errors.New("invalid affinity alias")
			}
			seen[alias] = true
			if len(seen) > maxEntries {
				return errors.New("affinity alias capacity exceeded")
			}
		}
	}
	return nil
}

// Restore adds validated unexpired hints without replacing any newer live alias.
func (c *SessionCache) Restore(groups []SessionBinding, now time.Time) error {
	if err := validateSessionBindings(groups, c.maxEntries); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureInitializedLocked()
	firstLive := c.evictionOrder.Front()
	remaining := c.maxEntries - len(c.entries)
	selected := make([]int, 0, len(groups))
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		if !now.Before(group.ExpiresAt) || len(group.Aliases) > remaining {
			continue
		}
		conflict := false
		for _, alias := range group.Aliases {
			if _, exists := c.entries[alias]; exists {
				conflict = true
				break
			}
		}
		if !conflict {
			selected = append(selected, i)
			remaining -= len(group.Aliases)
		}
	}
	for i := len(selected) - 1; i >= 0; i-- {
		group := groups[selected[i]]
		expiry := group.ExpiresAt
		if cap := now.Add(c.ttl); expiry.After(cap) {
			expiry = cap
		}
		if !now.Before(expiry) {
			continue
		}
		c.replaceAliasGroupsLocked(group.AuthID, expiry, group.Aliases)
		if firstLive != nil {
			c.evictionOrder.MoveBefore(c.evictionElements[group.Aliases[0]], firstLive)
		}
	}
	return nil
}
