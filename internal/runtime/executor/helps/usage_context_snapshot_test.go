package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type authenticationSnapshotPublication struct {
	ctx    context.Context
	record usage.Record
}

type authenticationSnapshotPlugin struct {
	requestID string
	received  chan authenticationSnapshotPublication
}

func (p *authenticationSnapshotPlugin) HandleUsage(ctx context.Context, record usage.Record) {
	if record.RequestID == p.requestID {
		p.received <- authenticationSnapshotPublication{ctx: ctx, record: record}
	}
}

func TestUsageReporterPublicationKeepsConstructionAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name     string
		apiKey   string
		provider string
		typed    bool
		native   bool
	}{
		{name: "empty_gin"},
		{name: "blank_gin", apiKey: " "},
		{name: "gin_plugin", apiKey: "synthetic-original-key", provider: "custom-auth-plugin"},
		{name: "typed_native", apiKey: "synthetic-typed-key", provider: "config-inline", typed: true, native: true},
		{name: "explicit_non_native", apiKey: "synthetic-original-key", provider: "config-inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
			defer cancel()
			ctx = context.WithValue(ctx, "gin", ginCtx)
			ctx = logging.WithResponseHeadersHolder(ctx)
			ctx = logging.WithRequestID(ctx, "snapshot-trace")
			ctx = usage.WithRequestedModelAlias(ctx, "snapshot-alias")
			ctx = usage.WithReasoningEffort(ctx, "high")
			ctx = usage.WithServiceTier(ctx, "priority")
			if tc.typed {
				ctx = usage.WithAPIKey(ctx, tc.apiKey)
				ctx = usage.WithAccessProvider(ctx, tc.provider)
			} else if tc.apiKey != "" {
				ginCtx.Set("userApiKey", tc.apiKey)
				ginCtx.Set("accessProvider", tc.provider)
			}
			ctx = usage.WithIsNativeKey(ctx, tc.native)
			reporter := NewUsageReporter(ctx, "snapshot-fixture", "synthetic-model", nil)
			plugin := &authenticationSnapshotPlugin{
				requestID: reporter.RequestID(), received: make(chan authenticationSnapshotPublication, 1),
			}
			usage.RegisterNamedPlugin("usage-authentication-snapshot-test", plugin)

			// A pooled Gin context now belongs to a different inbound request.
			ginCtx.Set("userApiKey", "synthetic-next-request-key")
			ginCtx.Set("accessProvider", "config-inline")
			logging.SetResponseHeaders(ctx, http.Header{"X-Synthetic-Upstream": {"late-header"}})
			cancel()
			publishCtx := ctx
			if tc.typed {
				publishCtx = usage.WithAPIKey(publishCtx, "synthetic-next-request-key")
				publishCtx = usage.WithAccessProvider(publishCtx, "next-auth-plugin")
				publishCtx = usage.WithIsNativeKey(publishCtx, false)
			}
			reporter.Publish(publishCtx, usage.Detail{TotalTokens: 1})
			select {
			case publication := <-plugin.received:
				if publication.record.APIKey != tc.apiKey || publication.record.AccessProvider != tc.provider || publication.record.IsNativeKey != tc.native {
					t.Fatal("usage publication changed construction-time authentication")
				}
				wantSource := strings.TrimSpace(tc.apiKey)
				if tc.typed {
					wantSource = ""
				}
				if publication.record.Source != wantSource {
					t.Fatal("usage snapshot changed legacy source attribution")
				}
				if publication.record.Model != "synthetic-model" || publication.record.Alias != "snapshot-alias" || publication.record.AuthID != "" || publication.record.ReasoningEffort != "high" || publication.record.ServiceTier != "priority" || publication.record.TraceID != "snapshot-trace" {
					t.Fatal("usage authentication snapshot changed unrelated record metadata")
				}
				if usage.APIKeyFromContext(publication.ctx) != strings.TrimSpace(tc.apiKey) || usage.AccessProviderFromContext(publication.ctx) != tc.provider {
					t.Fatal("usage plugin context did not preserve authentication snapshot")
				}
				if native, explicit := usage.IsNativeKeyFromContext(publication.ctx); !explicit || native != tc.native {
					t.Fatal("usage plugin context changed native-key attribution")
				}
				if publication.ctx.Value("gin") != nil {
					t.Fatal("usage publication retained pooled Gin metadata")
				}
				if publication.ctx.Err() != context.Canceled {
					t.Fatal("usage authentication snapshot lost cancellation")
				}
				if got, ok := publication.ctx.Deadline(); !ok {
					t.Fatal("usage authentication snapshot lost deadline")
				} else if want, _ := ctx.Deadline(); !got.Equal(want) {
					t.Fatal("usage authentication snapshot changed deadline")
				}
				if publication.record.ResponseHeaders.Get("X-Synthetic-Upstream") != "late-header" {
					t.Fatal("usage authentication snapshot lost late response headers")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("usage publication did not arrive")
			}
		})
	}
}
