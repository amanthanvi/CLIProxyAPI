package usage

import (
	"context"
	"testing"
	"time"
)

type legacyAuthenticationContext map[string]any

func (c legacyAuthenticationContext) Get(key string) (any, bool) {
	value, ok := c[key]
	return value, ok
}

func TestWithAuthenticationSnapshotKeepsExplicitEmpty(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	parent = context.WithValue(parent, "gin", legacyAuthenticationContext{
		"userApiKey": "synthetic-reused-key", "accessProvider": "config-inline",
	})
	type customKey struct{}
	parent = context.WithValue(parent, customKey{}, "preserved metadata")
	ctx := WithAuthenticationSnapshot(parent, "", "", false)
	if APIKeyFromContext(ctx) != "" || AccessProviderFromContext(ctx) != "" {
		t.Fatal("empty snapshot fell back to legacy authentication")
	}
	if value, present := IsNativeKeyFromContext(ctx); value || !present {
		t.Fatal("snapshot did not retain explicit non-native attribution")
	}
	if ctx.Value("gin") != nil || ctx.Value(customKey{}) != "preserved metadata" {
		t.Fatal("snapshot changed non-Gin metadata or retained Gin")
	}
	want, _ := parent.Deadline()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
		t.Fatal("snapshot changed context deadline")
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("snapshot did not propagate cancellation")
	}
}

func TestManagerPublishKeepsLegacyGinAuthenticationFallback(t *testing.T) {
	ctx := context.WithValue(context.Background(), "gin", legacyAuthenticationContext{
		"userApiKey": "synthetic-legacy-key", "accessProvider": "config-inline",
	})
	ctx = WithAPIKey(ctx, "")
	ctx = WithAccessProvider(ctx, "")
	manager := NewManager(1)
	t.Cleanup(manager.Stop)
	plugin := &captureUsagePlugin{records: make(chan Record, 1)}
	manager.Register(plugin)
	manager.Publish(ctx, Record{Provider: "legacy-fixture", Model: "synthetic-model"})
	select {
	case record := <-plugin.records:
		if record.APIKey != "synthetic-legacy-key" || record.AccessProvider != "config-inline" || !record.IsNativeKey {
			t.Fatal("legacy direct publication changed authentication fallback")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legacy direct publication did not arrive")
	}
}
