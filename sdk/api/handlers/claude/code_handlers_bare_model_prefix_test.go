package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/claude/models"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	bareMessagesPath    = "/v1/messages"
	bareCountTokensPath = "/v1/messages/count_tokens"
	bareRequestBetas    = "context-1m-2025-08-07,fast-mode-2026-02-01"
	barePoolA           = "bare-prefix-pool-a"
	barePoolB           = "bare-prefix-pool-b"
	bareZenAuth         = "bare-prefix-zen"
	bareUnprefixedAuth  = "bare-prefix-unprefixed"
)

// bareForwardedHeaders are client headers the executor must receive unchanged.
var bareForwardedHeaders = []string{"Anthropic-Beta", "Anthropic-Version", "Content-Type", "X-Bare-Prefix-Fixture"}

type bareModelCall struct {
	kind    string
	authID  string
	model   string
	payload string
	headers http.Header
}

type bareModelRecordingExecutor struct {
	mu    sync.Mutex
	calls []bareModelCall
}

func (e *bareModelRecordingExecutor) Identifier() string { return "claude" }

func (e *bareModelRecordingExecutor) record(kind string, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	call := bareModelCall{kind: kind, model: req.Model, payload: string(req.Payload), headers: opts.Headers.Clone()}
	if auth != nil {
		call.authID = auth.ID
	}
	e.calls = append(e.calls, call)
}

func (e *bareModelRecordingExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record("execute", auth, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"id":"msg_bare_prefix","type":"message","role":"assistant","content":[]}`)}, nil
}

func (e *bareModelRecordingExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record("stream", auth, req, opts)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *bareModelRecordingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *bareModelRecordingExecutor) CountTokens(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record("count", auth, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":7}`)}, nil
}

func (e *bareModelRecordingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("network is not used by this fixture")
}

func (e *bareModelRecordingExecutor) take() []bareModelCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	calls := e.calls
	e.calls = nil
	return calls
}

type bareModelFixture struct {
	base     *handlers.BaseAPIHandler
	executor *bareModelRecordingExecutor
	router   *gin.Engine
}

// newBareModelFixture mirrors force-model-prefix registration: pooled Claude credentials
// expose only "<prefix>/<model>" routes, while other credentials and providers own
// look-alike routes that a bare model must never reach.
func newBareModelFixture(t *testing.T, prefix string) *bareModelFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &bareModelRecordingExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	t.Cleanup(manager.StopAutoRefresh)

	modelRegistry := registry.GetGlobalRegistry()
	register := func(clientID, provider, authPrefix string, withAuth bool, models ...string) {
		t.Helper()
		if withAuth {
			auth := &coreauth.Auth{ID: clientID, Provider: provider, Prefix: authPrefix, Status: coreauth.StatusActive}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth %s: %v", clientID, errRegister)
			}
		}
		infos := make([]*registry.ModelInfo, 0, len(models))
		for _, model := range models {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		modelRegistry.RegisterClient(clientID, provider, infos)
		t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })
	}
	register(barePoolA, "claude", "claude", true,
		"claude/claude-opus-5-5", "claude/claude-bare-route-5", "claude/gpt-bare-prefix-5",
		// Targets that an unvalidated prefix would reach.
		"cla ude/claude-opus-5-5", "claude?x/claude-opus-5-5", "claude#x/claude-opus-5-5",
		`claude\x/claude-opus-5-5`, "claude/x/claude-opus-5-5", "/claude/claude-opus-5-5",
		"../claude-opus-5-5", "./claude-opus-5-5")
	register(barePoolB, "claude", "claude", true, "claude/claude-opus-5-5")
	register(bareZenAuth, "claude", "zen", true, "zen/claude-opus-5")
	register(bareUnprefixedAuth, "claude", "", true, "claude-bare-route-5")
	register("bare-prefix-openrouter", "openrouter", "", false, "claude/claude-wrong-provider-5", "claude/claude-ambiguous-5")
	register("bare-prefix-claude-registry", "claude", "", false, "claude/claude-ambiguous-5")

	cfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{DisableCloakingModelList: true, BareModelPrefix: prefix}}
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	handler := NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.POST(bareMessagesPath, handler.ClaudeMessages)
	router.POST(bareCountTokensPath, handler.ClaudeCountTokens)
	router.GET("/v1/models", handler.ClaudeModels)
	return &bareModelFixture{base: base, executor: executor, router: router}
}

func (f *bareModelFixture) post(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", bareRequestBetas)
	req.Header.Set("X-Bare-Prefix-Fixture", "kept")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, req)
	return recorder
}

// postOne sends a request that must reach the executor exactly once.
func (f *bareModelFixture) postOne(t *testing.T, path, body string) bareModelCall {
	t.Helper()
	recorder := f.post(path, body)
	calls := f.executor.take()
	if recorder.Code != http.StatusOK || len(calls) != 1 {
		t.Fatalf("POST %s %s: HTTP %d with %d executor calls; body %s", path, body, recorder.Code, len(calls), recorder.Body.String())
	}
	return calls[0]
}

// postStandardOne sends a request without Anthropic-Beta that must reach the executor once.
func (f *bareModelFixture) postStandardOne(t *testing.T, path, body string) bareModelCall {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, req)
	calls := f.executor.take()
	if recorder.Code != http.StatusOK || len(calls) != 1 {
		t.Fatalf("POST %s %s: HTTP %d with %d executor calls; body %s", path, body, recorder.Code, len(calls), recorder.Body.String())
	}
	return calls[0]
}

// postRejected sends a request that must fail before reaching the executor. An empty
// wantMessage only requires an error status.
func (f *bareModelFixture) postRejected(t *testing.T, path, body, wantMessage string) {
	t.Helper()
	recorder := f.post(path, body)
	if calls := f.executor.take(); len(calls) != 0 {
		t.Fatalf("POST %s %s reached the executor: %+v", path, body, calls)
	}
	if wantMessage == "" {
		if recorder.Code < http.StatusBadRequest {
			t.Fatalf("POST %s %s: HTTP %d, want an error status", path, body, recorder.Code)
		}
		return
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST %s %s: HTTP %d, want 400; body %s", path, body, recorder.Code, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "error.message").String(); got != wantMessage {
		t.Fatalf("POST %s %s: error message %q, want %q", path, body, got, wantMessage)
	}
}

func bareModelBody(model string, stream bool) string {
	encoded, _ := json.Marshal(model)
	body := `{"model":` + string(encoded) + `,"max_tokens":64,"speed":"fast","output_config":{"effort":"high"},` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]`
	if stream {
		body += `,"stream":true`
	}
	return body + "}"
}

// assertSameAsQualified requires a bare request to reach the same credential pool with
// the same upstream model, payload, and client headers as its explicitly qualified form.
func assertSameAsQualified(t *testing.T, bare, qualified bareModelCall) {
	t.Helper()
	if bare.kind != qualified.kind || bare.model != qualified.model || bare.payload != qualified.payload {
		t.Fatalf("bare request differs from its qualified form:\nbare      %s %q %s\nqualified %s %q %s", bare.kind, bare.model, bare.payload, qualified.kind, qualified.model, qualified.payload)
	}
	if bare.authID != barePoolA && bare.authID != barePoolB {
		t.Fatalf("bare request used auth %q, want the claude pool", bare.authID)
	}
	if got, want := bareHeaderNames(bare.headers), bareHeaderNames(qualified.headers); !reflect.DeepEqual(got, want) {
		t.Fatalf("bare request headers %v, want %v", got, want)
	}
	for _, name := range bareForwardedHeaders {
		if got, want := bare.headers.Values(name), qualified.headers.Values(name); len(got) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("header %s = %q, want %q", name, got, want)
		}
	}
}

func bareHeaderNames(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func bareCatalogIDs(t *testing.T, f *bareModelFixture) []string {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: HTTP %d", recorder.Code)
	}
	var ids []string
	for _, id := range gjson.Get(recorder.Body.String(), "data.#.id").Array() {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	return ids
}

func TestClaudeBareModelPrefixRoutesLikeQualifiedRequest(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		stream bool
		kind   string
	}{
		{name: "messages", path: bareMessagesPath, kind: "execute"},
		{name: "messages stream", path: bareMessagesPath, stream: true, kind: "stream"},
		{name: "count tokens", path: bareCountTokensPath, kind: "count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBareModelFixture(t, "claude")
			qualified := f.postOne(t, tc.path, bareModelBody("claude/claude-opus-5-5", tc.stream))
			bare := f.postOne(t, tc.path, bareModelBody("claude-opus-5-5", tc.stream))
			assertSameAsQualified(t, bare, qualified)
			if bare.kind != tc.kind || bare.model != "claude-opus-5-5" {
				t.Fatalf("executor call = %s %q, want %s %q", bare.kind, bare.model, tc.kind, "claude-opus-5-5")
			}
			if got := bare.headers.Get("Anthropic-Beta"); got != bareRequestBetas {
				t.Fatalf("Anthropic-Beta = %q, want %q", got, bareRequestBetas)
			}
			for field, want := range map[string]string{
				"model":                "claude/claude-opus-5-5",
				"speed":                "fast",
				"output_config.effort": "high",
				"tools.0.name":         "lookup",
				"messages.0.content":   "hi",
			} {
				if got := gjson.Get(bare.payload, field).String(); got != want {
					t.Fatalf("payload %s = %q, want %q; payload %s", field, got, want, bare.payload)
				}
			}
		})
	}
}

func TestClaudeBareModelPrefixKeepsThinkingSuffixAndDDDecoding(t *testing.T) {
	f := newBareModelFixture(t, "claude")
	qualified := f.postOne(t, bareMessagesPath, bareModelBody("claude/claude-opus-5-5(high)", false))
	bare := f.postOne(t, bareMessagesPath, bareModelBody("claude-opus-5-5(high)", false))
	assertSameAsQualified(t, bare, qualified)
	if got := gjson.Get(bare.payload, "model").String(); got != "claude/claude-opus-5-5(high)" {
		t.Fatalf("payload model = %q, want the full thinking suffix", got)
	}

	const encoded = "claude-fable-5-dd-5-5-supo-edualc"
	if decoded := claudemodels.ResolveClaudeModelIDPrefix(encoded); decoded != "claude-opus-5-5" {
		t.Fatalf("fixture %q decodes to %q", encoded, decoded)
	}
	plain := f.postOne(t, bareCountTokensPath, bareModelBody("claude/claude-opus-5-5", false))
	decodedCall := f.postOne(t, bareCountTokensPath, bareModelBody(encoded, false))
	assertSameAsQualified(t, decodedCall, plain)
}

func TestClaudeBareModelPrefixLeavesOtherModelsUnchanged(t *testing.T) {
	f := newBareModelFixture(t, "claude")
	for _, path := range []string{bareMessagesPath, bareCountTokensPath} {
		for _, model := range []string{
			"claude-opus-5",           // only zen/claude-opus-5 exists; no cross-prefix fallback
			"claude-wrong-provider-5", // claude/ target is owned by another provider
			"claude-ambiguous-5",      // claude/ target is shared with another provider
			"claude-missing-5(high)",  // no claude/ target
			"claude-opus-5-5[1m]",     // a literal context suffix is not an invented route
			"gpt-bare-prefix-5",       // not a claude-* model
			"Claude-opus-5-5",         // the family match is exact
		} {
			f.postRejected(t, path, bareModelBody(model, false), "unknown provider for model "+model)
		}
		for _, body := range []string{
			`{"model":["claude-opus-5-5"],"messages":[]}`,
			`{"model":{"id":"claude-opus-5-5"},"messages":[]}`,
			`{"model":null,"messages":[]}`,
			`{"messages":[{"role":"user","content":"hi"}]}`,
			`{"model":"claude-opus-5-5","messages":[`,
			`{"model":"claude-opus-5-5","messages":[]}trailing`,
		} {
			f.postRejected(t, path, body, "")
		}
	}

	bareRoute := f.postOne(t, bareMessagesPath, bareModelBody("claude-bare-route-5", false))
	if bareRoute.authID != bareUnprefixedAuth || gjson.Get(bareRoute.payload, "model").String() != "claude-bare-route-5" {
		t.Fatalf("existing bare route changed: auth %q payload %s", bareRoute.authID, bareRoute.payload)
	}
	zen := f.postOne(t, bareMessagesPath, bareModelBody("zen/claude-opus-5", false))
	if zen.authID != bareZenAuth || zen.model != "claude-opus-5" || gjson.Get(zen.payload, "model").String() != "zen/claude-opus-5" {
		t.Fatalf("zen route changed: auth %q model %q payload %s", zen.authID, zen.model, zen.payload)
	}

	// auto keeps its existing resolution; its payload model is never qualified.
	f.post(bareMessagesPath, bareModelBody("auto", false))
	for _, call := range f.executor.take() {
		if got := gjson.Get(call.payload, "model").String(); got != "auto" {
			t.Fatalf("auto payload model = %q, want auto", got)
		}
	}
}

func TestClaudeBareModelPrefixRequiresOneValidSegment(t *testing.T) {
	for _, prefix := range []string{"", "   ", "claude/x", "/claude", "cla ude", "claude?x", "claude#x", `claude\x`, ".", "..", "../claude"} {
		t.Run(fmt.Sprintf("%q", prefix), func(t *testing.T) {
			f := newBareModelFixture(t, prefix)
			for _, path := range []string{bareMessagesPath, bareCountTokensPath} {
				f.postRejected(t, path, bareModelBody("claude-opus-5-5", false), "unknown provider for model claude-opus-5-5")
			}
		})
	}
	t.Run("trimmed", func(t *testing.T) {
		f := newBareModelFixture(t, " claude ")
		call := f.postOne(t, bareCountTokensPath, bareModelBody("claude-opus-5-5", false))
		if got := gjson.Get(call.payload, "model").String(); got != "claude/claude-opus-5-5" {
			t.Fatalf("payload model = %q, want claude/claude-opus-5-5", got)
		}
	})
}

func TestClaudeBareModelPrefixFollowsConfigUpdates(t *testing.T) {
	f := newBareModelFixture(t, "claude")
	body := bareModelBody("claude-opus-5-5", false)
	f.postOne(t, bareMessagesPath, body)
	f.base.UpdateClients(&sdkconfig.SDKConfig{})
	f.postRejected(t, bareMessagesPath, body, "unknown provider for model claude-opus-5-5")
	f.base.UpdateClients(&sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{BareModelPrefix: "claude"}})
	f.postOne(t, bareMessagesPath, body)
}

// TestClaudeBareModelPrefixStandardRequestAddsNoBetaOrOptions covers a standard-context
// request without context or fast betas: qualification must not invent headers or fields.
func TestClaudeBareModelPrefixStandardRequestAddsNoBetaOrOptions(t *testing.T) {
	const fields = `,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name   string
		path   string
		stream bool
	}{
		{name: "messages", path: bareMessagesPath},
		{name: "messages stream", path: bareMessagesPath, stream: true},
		{name: "count tokens", path: bareCountTokensPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streamField := ""
			if tc.stream {
				streamField = `,"stream":true`
			}
			f := newBareModelFixture(t, "claude")
			qualified := f.postStandardOne(t, tc.path, `{"model":"claude/claude-opus-5-5"`+fields+streamField+`}`)
			bare := f.postStandardOne(t, tc.path, `{"model":"claude-opus-5-5"`+fields+streamField+`}`)
			if bare.kind != qualified.kind || bare.model != qualified.model || bare.payload != qualified.payload {
				t.Fatalf("bare request differs from its qualified form:\nbare      %s %q %s\nqualified %s %q %s", bare.kind, bare.model, bare.payload, qualified.kind, qualified.model, qualified.payload)
			}
			if bare.authID != barePoolA && bare.authID != barePoolB {
				t.Fatalf("bare request used auth %q, want the claude pool", bare.authID)
			}
			if got := gjson.Get(bare.payload, "model").String(); got != "claude/claude-opus-5-5" {
				t.Fatalf("payload model = %q, want claude/claude-opus-5-5", got)
			}
			allowed := map[string]bool{"model": true, "max_tokens": true, "messages": true, "stream": tc.stream}
			gjson.Parse(bare.payload).ForEach(func(key, _ gjson.Result) bool {
				if !allowed[key.String()] {
					t.Fatalf("payload gained field %q: %s", key.String(), bare.payload)
				}
				return true
			})
			if got := bare.headers.Values("Anthropic-Beta"); len(got) != 0 {
				t.Fatalf("Anthropic-Beta = %q, want none", got)
			}
			if got, want := bareHeaderNames(bare.headers), bareHeaderNames(qualified.headers); !reflect.DeepEqual(got, want) {
				t.Fatalf("bare request headers %v, want %v", got, want)
			}
		})
	}
}

func TestClaudeBareModelPrefixSkipsHomeMode(t *testing.T) {
	f := newBareModelFixture(t, "claude")
	handler := NewClaudeCodeAPIHandler(f.base)
	body := []byte(bareModelBody("claude-opus-5-5", false))
	if got := gjson.GetBytes(handler.qualifyBareClaudeModelInBody(body), "model").String(); got != "claude/claude-opus-5-5" {
		t.Fatalf("control model = %q, want claude/claude-opus-5-5", got)
	}
	f.base.AuthManager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	if !f.base.AuthManager.HomeEnabled() {
		t.Fatal("fixture did not enable Home mode")
	}
	if got := handler.qualifyBareClaudeModelInBody(body); string(got) != string(body) {
		t.Fatalf("Home mode rewrote the body: %s", got)
	}
}

type bareModelRouterHost struct {
	active    bool
	mu        sync.Mutex
	requested []string
}

func (r *bareModelRouterHost) RouteModel(_ context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requested = append(r.requested, req.RequestedModel)
	return pluginapi.ModelRouteResponse{}, false
}

func (r *bareModelRouterHost) HasModelRouters() bool { return r.active }

func (r *bareModelRouterHost) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requested...)
}

func TestClaudeBareModelPrefixLeavesActiveModelRouterRequestsBare(t *testing.T) {
	body := bareModelBody("claude-opus-5-5", false)
	f := newBareModelFixture(t, "claude")

	inactive := &bareModelRouterHost{}
	f.base.SetModelRouterHost(inactive)
	if call := f.postOne(t, bareMessagesPath, body); gjson.Get(call.payload, "model").String() != "claude/claude-opus-5-5" {
		t.Fatalf("inactive model router blocked qualification: payload %s", call.payload)
	}
	if seen := inactive.seen(); len(seen) != 0 {
		t.Fatalf("inactive model router was consulted: %v", seen)
	}

	active := &bareModelRouterHost{active: true}
	f.base.SetModelRouterHost(active)
	for _, path := range []string{bareMessagesPath, bareCountTokensPath} {
		f.postRejected(t, path, body, "unknown provider for model claude-opus-5-5")
	}
	if seen := active.seen(); !reflect.DeepEqual(seen, []string{"claude-opus-5-5", "claude-opus-5-5"}) {
		t.Fatalf("active model router saw %v, want the bare model on both endpoints", seen)
	}
}

func TestClaudeBareModelPrefixKeepsQualifiedRoutesCatalogAndOtherEntrypoints(t *testing.T) {
	f := newBareModelFixture(t, "claude")
	enabledCfg := f.base.Cfg
	disabledCfg := &sdkconfig.SDKConfig{ClaudeCode: sdkconfig.ClaudeCodeConfig{DisableCloakingModelList: true}}
	for _, model := range []string{"claude/claude-opus-5-5", "zen/claude-opus-5"} {
		body := bareModelBody(model, false)
		enabled := f.postOne(t, bareMessagesPath, body)
		f.base.UpdateClients(disabledCfg)
		disabled := f.postOne(t, bareMessagesPath, body)
		f.base.UpdateClients(enabledCfg)
		if enabled.payload != body || disabled.payload != body || enabled.model != disabled.model {
			t.Fatalf("qualified %s changed: enabled %q %s, disabled %q %s", model, enabled.model, enabled.payload, disabled.model, disabled.payload)
		}
	}

	catalog := bareCatalogIDs(t, f)
	f.post(bareMessagesPath, bareModelBody("claude-opus-5-5", false))
	f.executor.take()
	if after := bareCatalogIDs(t, f); !reflect.DeepEqual(after, catalog) {
		t.Fatalf("catalog changed after a bare request:\nbefore %v\nafter  %v", catalog, after)
	}
	sawQualified := false
	for _, id := range catalog {
		if id == "claude-opus-5-5" {
			t.Fatalf("catalog advertises the bare model: %v", catalog)
		}
		sawQualified = sawQualified || id == "claude/claude-opus-5-5"
	}
	if !sawQualified {
		t.Fatalf("catalog lacks claude/claude-opus-5-5: %v", catalog)
	}

	// Other entry protocols keep strict prefix routing for bare models.
	for _, handlerType := range []string{"openai", "openai-response"} {
		_, _, errMsg := f.base.ExecuteWithAuthManager(context.Background(), handlerType, "claude-opus-5-5", []byte(`{"model":"claude-opus-5-5"}`), "")
		if errMsg == nil || errMsg.Error == nil || errMsg.StatusCode != http.StatusBadRequest || !strings.Contains(errMsg.Error.Error(), "unknown provider for model claude-opus-5-5") {
			t.Fatalf("%s accepted a bare model: %+v", handlerType, errMsg)
		}
	}
	if _, _, errMsg := f.base.ExecuteCountWithAuthManager(context.Background(), "openai", "claude-opus-5-5", []byte(`{"model":"claude-opus-5-5"}`), ""); errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("openai count accepted a bare model: %+v", errMsg)
	}
	if calls := f.executor.take(); len(calls) != 0 {
		t.Fatalf("strict entrypoints reached the executor: %+v", calls)
	}
}
