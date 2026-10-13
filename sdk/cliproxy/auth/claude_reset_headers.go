package auth

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const claudeResetWindow = 7*24*time.Hour + time.Hour

// FilterClaudeRateLimitResponseHeaders preserves native reset provenance, never a scheduler retry deadline.
func FilterClaudeRateLimitResponseHeaders(headers http.Header, now time.Time) http.Header {
	value := func(name string) string {
		var found string
		matches := 0
		for key, values := range headers {
			if strings.EqualFold(key, name) {
				matches++
				if len(values) != 1 {
					return ""
				}
				found = values[0]
			}
		}
		if matches != 1 {
			return ""
		}
		return found
	}
	if ambiguousClaudeResetHeaders(headers) {
		return nil
	}
	status := value("Anthropic-Ratelimit-Unified-Status")
	claim := value("Anthropic-Ratelimit-Unified-Representative-Claim")
	switch claim {
	case "five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included":
	default:
		return nil
	}
	reset := value("Anthropic-Ratelimit-Unified-Reset")
	resetAt, ok := claudeNativeReset(reset, now)
	if status != "rejected" || !ok {
		return nil
	}
	claimedWindow := map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_overage_included": "7d_oi"}[claim]
	if claimedWindow != "" {
		windowStatus := value("Anthropic-Ratelimit-Unified-" + claimedWindow + "-Status")
		if windowStatus != "" && windowStatus != "rejected" {
			return nil
		}
		windowReset := value("Anthropic-Ratelimit-Unified-" + claimedWindow + "-Reset")
		if windowReset != "" {
			windowAt, valid := claudeNativeReset(windowReset, now)
			if !valid || !windowAt.Equal(resetAt) {
				return nil
			}
		}
	}
	overage := value("Anthropic-Ratelimit-Unified-Overage-Status")
	if overage != "" && overage != "allowed" && overage != "allowed_warning" && overage != "rejected" {
		return nil
	}
	for _, window := range []string{"5h", "7d", "7d_oi"} {
		windowStatus := value("Anthropic-Ratelimit-Unified-" + window + "-Status")
		if windowStatus != "" && windowStatus != "allowed" && windowStatus != "allowed_warning" && windowStatus != "rejected" {
			return nil
		}
		if windowStatus != "rejected" {
			continue
		}
		raw := value("Anthropic-Ratelimit-Unified-" + window + "-Reset")
		if raw == "" {
			continue
		}
		windowAt, valid := claudeNativeReset(raw, now)
		if !valid || windowAt.After(resetAt) {
			return nil
		}
	}
	out := http.Header{
		"Anthropic-Ratelimit-Unified-Status":               {status},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {claim},
		"Anthropic-Ratelimit-Unified-Reset":                {reset},
	}
	if overage != "" {
		out.Set("Anthropic-Ratelimit-Unified-Overage-Status", overage)
	}
	return out
}

func ambiguousClaudeResetHeaders(headers http.Header) bool {
	seen := make(map[string]bool)
	for key, values := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(lower, "anthropic-ratelimit-unified-") {
			continue
		}
		if key != strings.TrimSpace(key) || seen[lower] || len(values) != 1 {
			return true
		}
		seen[lower] = true
	}
	return false
}

func claudeNativeReset(raw string, now time.Time) (time.Time, bool) {
	if raw == "" || len(raw) > 32 || strings.TrimSpace(raw) != raw {
		return time.Time{}, false
	}
	for _, ch := range raw {
		if (ch < '0' || ch > '9') && ch != '.' {
			return time.Time{}, false
		}
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, false
	}
	// Bound the number before converting to int64 or applying time arithmetic.
	if seconds <= float64(now.Unix()) || seconds > float64(now.Add(claudeResetWindow).Unix()) {
		return time.Time{}, false
	}
	reset := time.Unix(int64(math.Round(seconds)), 0)
	return reset, reset.After(now)
}

// SafeClaudeRateLimitResponseHeaders validates even typed error metadata at the response boundary.
func SafeClaudeRateLimitResponseHeaders(err error, now time.Time) http.Header {
	var source interface{ ClaudeRateLimitResponseHeaders() http.Header }
	if !errors.As(err, &source) || source == nil {
		return nil
	}
	return FilterClaudeRateLimitResponseHeaders(source.ClaudeRateLimitResponseHeaders(), now)
}

func claudeResetHeadersForAuth(auth *Auth, model string, now time.Time) http.Header {
	if auth == nil || auth.Provider != "claude" {
		return nil
	}
	quota, lastErr, updatedAt := auth.Quota, auth.LastError, auth.UpdatedAt
	if quota.Reason != "credential_quota" && len(auth.ModelStates) > 0 {
		state := existingModelState(auth, canonicalModelKey(model))
		if state == nil {
			return nil
		}
		quota, lastErr, updatedAt = state.Quota, state.LastError, state.UpdatedAt
	}
	if !quota.Exceeded || lastErr == nil || lastErr.HTTPStatus != http.StatusTooManyRequests || quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) || quota.ObservedAt.Before(updatedAt) {
		return nil
	}
	headers := make(http.Header, len(quota.Signals))
	for key, value := range quota.Signals {
		headers.Set(key, value)
	}
	return FilterClaudeRateLimitResponseHeaders(headers, now)
}

func claudeResetHeadersForPool(auths []*Auth, modelForAuth func(*Auth) string, earliest, now time.Time) http.Header {
	var chosen *Auth
	var chosenModel string
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		model := modelForAuth(candidate)
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked || reason != blockReasonCooldown || !next.Equal(earliest) {
			continue
		}
		if chosen == nil || authPriority(candidate) > authPriority(chosen) || authPriority(candidate) == authPriority(chosen) && candidate.ID < chosen.ID {
			chosen, chosenModel = candidate, model
		}
	}
	return claudeResetHeadersForAuth(chosen, chosenModel, now)
}

func (m *modelScheduler) claudeResetHeadersLocked(earliest, now time.Time, predicate func(*scheduledAuth) bool) http.Header {
	var candidates []*Auth
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil || entry.state != scheduledStateCooldown || !entry.nextRetryAt.Equal(earliest) || predicate != nil && !predicate(entry) {
			continue
		}
		candidates = append(candidates, entry.auth)
	}
	return claudeResetHeadersForPool(candidates, func(*Auth) string { return m.modelKey }, earliest, now)
}
