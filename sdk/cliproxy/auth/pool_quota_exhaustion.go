package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// PoolQuotaExhaustion records local admission state, not a provider reset promise.
type PoolQuotaExhaustion struct {
	EligibleCredentials int
	ObservedAt          time.Time
	EarliestAdmission   time.Time
}

// PoolQuotaExhaustionDetails returns only Manager-certified pool observations.
func PoolQuotaExhaustionDetails(err error) (PoolQuotaExhaustion, bool) {
	var exhausted *poolQuotaExhaustionError
	if !errors.As(err, &exhausted) || exhausted == nil {
		return PoolQuotaExhaustion{}, false
	}
	return exhausted.details, true
}

type poolQuotaExhaustionError struct {
	cause   error
	details PoolQuotaExhaustion
}

func (e *poolQuotaExhaustionError) Error() string   { return e.cause.Error() }
func (e *poolQuotaExhaustionError) Unwrap() error   { return e.cause }
func (e *poolQuotaExhaustionError) StatusCode() int { return statusCodeFromError(e.cause) }
func (e *poolQuotaExhaustionError) Headers() http.Header {
	var source interface{ Headers() http.Header }
	if errors.As(e.cause, &source) {
		return source.Headers()
	}
	return nil
}
func (e *poolQuotaExhaustionError) RetryAfter() *time.Duration { return retryAfterFromError(e.cause) }

func (m *Manager) certifyPoolQuotaExhaustion(ctx context.Context, providers []string, routeModel string, opts cliproxyexecutor.Options, cause error) error {
	if m == nil || cause == nil || statusCodeFromError(cause) != http.StatusTooManyRequests || m.HomeEnabled() ||
		pinnedAuthIDFromMetadata(opts.Metadata) != "" || strings.TrimSpace(routeModel) == "" ||
		isRequestStopError(cause) || isRequestInvalidError(cause) || shouldSkipCredentialCooldown(resultErrorFromError(cause)) ||
		(ctx != nil && ctx.Err() != nil) {
		return cause
	}
	configSnapshot := m.runtimeConfig.Load()
	aliasSnapshot := m.oauthModelAlias.Load()
	routingSnapshot := m.apiKeyModelRouting.Load()
	routing := m.loadAPIKeyModelRouting()
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range m.normalizeProviders(providers) {
		providerSet[provider] = struct{}{}
	}
	if len(providerSet) == 0 {
		return cause
	}
	reg := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	knownSelector, positiveWeight := poolQuotaSelectorSemantics(m.selector)
	if !knownSelector {
		return cause
	}
	for provider := range providerSet {
		if m.executors[provider] == nil {
			return cause
		}
	}
	now := time.Now()
	details := PoolQuotaExhaustion{ObservedAt: now}
	registrationEpochs := make(map[string]uint64)
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !eligibility.allows(auth) {
			continue
		}
		if _, ok := providerSet[executorKeyFromAuth(auth)]; !ok {
			continue
		}
		if positiveWeight && authWeight(auth) <= 0 {
			continue
		}
		_, epoch := reg.GetModelsAndEpochForClient(auth.ID)
		registrationEpochs[auth.ID] = epoch
		if !m.authSupportsRouteModel(reg, auth, routeModel) {
			continue
		}
		// Resolve the same alias path without rotating a configured model pool.
		requested := rewriteModelForAuth(routeModel, auth)
		alias := m.resolveExecutionAliasResultForRequestedWithRouting(routing, auth, requested)
		upstream := executionAliasPoolModel(auth, requested, alias)
		pool := resolveOpenAICompatUpstreamModelPool(routing.config, auth, upstream)
		if len(pool) > 1 {
			return cause
		}
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
			return cause
		}
		if len(pool) == 1 {
			upstream = pool[0]
		} else {
			upstream = m.applyAPIKeyModelAliasWithRouting(routing, auth, upstream)
		}
		stateModel := m.stateModelForExecution(auth, routeModel, upstream, false)
		admission, quotaOnly := poolQuotaAdmission(auth, stateModel, now)
		if !quotaOnly {
			return cause
		}
		details.EligibleCredentials++
		if details.EarliestAdmission.IsZero() || admission.Before(details.EarliestAdmission) {
			details.EarliestAdmission = admission
		}
	}
	if details.EligibleCredentials == 0 || configSnapshot != m.runtimeConfig.Load() || aliasSnapshot != m.oauthModelAlias.Load() || routingSnapshot != m.apiKeyModelRouting.Load() {
		return cause
	}
	// Registration epochs also cover currently unsupported credentials becoming eligible.
	for id, epoch := range registrationEpochs {
		if _, current := reg.GetModelsAndEpochForClient(id); current != epoch {
			return cause
		}
	}
	if !details.EarliestAdmission.After(time.Now()) {
		return cause
	}
	return &poolQuotaExhaustionError{cause: cause, details: details}
}

func poolQuotaSelectorSemantics(selector Selector) (known, positiveWeight bool) {
	switch s := selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true, false
	case *WeightedRoundRobinSelector:
		return true, true
	case *SessionAffinitySelector:
		if s == nil {
			return false, false
		}
		return poolQuotaSelectorSemantics(s.fallback)
	default:
		return false, false
	}
}

func poolQuotaAdmission(auth *Auth, model string, now time.Time) (time.Time, bool) {
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if !blocked || reason != blockReasonCooldown || !next.After(now) || !poolQuotaLastError(auth.LastError) {
		return time.Time{}, false
	}
	quotaActive := auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
	if len(auth.ModelStates) == 0 {
		quotaActive = quotaActive || auth.Quota.Exceeded && auth.Quota.Reason == "quota" && auth.Quota.NextRecoverAt.After(now)
	}
	for key, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(key) != canonicalModelKey(model) {
			continue
		}
		if state.Status == StatusDisabled || !poolQuotaLastError(state.LastError) {
			return time.Time{}, false
		}
		stateBlocked, _, stateNext := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
		if !stateBlocked {
			continue
		}
		if !state.Quota.Exceeded || (state.Quota.Reason != "quota" && state.Quota.Reason != "credential_quota") || !state.Quota.NextRecoverAt.After(now) || !stateNext.After(now) {
			return time.Time{}, false
		}
		quotaActive = true
		if stateNext.After(next) {
			next = stateNext
		}
	}
	return next, quotaActive
}

func poolQuotaLastError(last *Error) bool {
	// A credential's unrelated model error may conservatively suppress certification.
	// It must never be hidden by an older credential-wide quota park.
	return last == nil || last.HTTPStatus == http.StatusTooManyRequests && !shouldSkipCredentialCooldown(last)
}
