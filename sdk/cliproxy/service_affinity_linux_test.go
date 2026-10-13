//go:build linux

package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type affinityLifecycleStore struct{ auths []*coreauth.Auth }

func (s *affinityLifecycleStore) List(context.Context) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, len(s.auths))
	for _, auth := range s.auths {
		out = append(out, auth.Clone())
	}
	return out, nil
}
func (*affinityLifecycleStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }
func (*affinityLifecycleStore) Delete(context.Context, string) error                 { return nil }

type affinityLifecycleExecutor struct {
	entered chan struct{}
	release chan struct{}
}

func (*affinityLifecycleExecutor) Identifier() string { return "affinity-test" }
func (e *affinityLifecycleExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth.ID == "synthetic-a" {
		close(e.entered)
		<-e.release
		return cliproxyexecutor.Response{}, affinityLifecycleQuota{}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}
func (*affinityLifecycleExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not used")
}
func (*affinityLifecycleExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*affinityLifecycleExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*affinityLifecycleExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

type affinityLifecycleQuota struct{}

func (affinityLifecycleQuota) Error() string   { return "synthetic quota exhausted" }
func (affinityLifecycleQuota) StatusCode() int { return http.StatusTooManyRequests }

func serviceAffinityFixture(t *testing.T) (*Service, *coreauth.FileAffinityStore, *coreauth.SessionAffinitySelector, cliproxyexecutor.Options) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.SessionAffinity, cfg.Routing.SessionAffinityTTL = true, "1h"
	cfg.Routing.SessionAffinityStatePath = filepath.Join(dir, "session-affinity.json")
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{TTL: time.Hour, Fallback: &coreauth.FillFirstSelector{}, RebindOn: "quota"})
	t.Cleanup(selector.Stop)
	store := &affinityLifecycleStore{auths: []*coreauth.Auth{
		{ID: "synthetic-a", Provider: "affinity-test", Status: coreauth.StatusActive, Metadata: map[string]any{"account_id": "a"}},
		{ID: "synthetic-b", Provider: "affinity-test", Status: coreauth.StatusActive, Metadata: map[string]any{"account_id": "b"}, Attributes: map[string]string{"priority": "9"}},
	}}
	manager := coreauth.NewManager(store, selector, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"synthetic-thread"}}, Metadata: map[string]any{}}
	// Manager.Execute uses the native mixed-provider affinity namespace.
	if _, err := selector.Pick(context.Background(), "mixed", "synthetic-model", opts, store.auths[:1]); err != nil {
		t.Fatal(err)
	}
	fileStore, err := coreauth.NewFileAffinityStore(cfg.Routing.SessionAffinityStatePath, cfg.AuthDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{cfg: cfg, coreManager: manager}, fileStore, selector, opts
}

func TestServiceAffinityPeriodicCheckpointAndHotReload(t *testing.T) {
	service, store, _, _ := serviceAffinityFixture(t)
	service.configureAffinityStateStore(service.cfg, false)
	if service.affinityStore != nil {
		t.Fatal("pre-start runtime config started persistence")
	}
	service.configureAffinityStateStore(service.cfg, true)
	if service.affinityStop == nil {
		t.Fatal("startup did not start writer")
	}
	service.stopAffinityCheckpoints()
	ticks, stop, done := make(chan time.Time), make(chan struct{}), make(chan struct{})
	go func() { defer close(done); service.runAffinityCheckpointTicks(stop, ticks) }()
	ticks <- time.Now()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if snapshot, err := store.Load(); err == nil && len(snapshot.Bindings) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic tick did not persist hints")
		}
		runtime.Gosched()
	}
	close(stop)
	<-done
	service.affinityStopping = false
	oldSelector := service.coreManager.Selector()
	newCfg := *service.cfg
	newCfg.Routing.Strategy = "round-robin"
	newCfg.Routing.SessionAffinityStatePath = "relative-invalid"
	if !service.applyManagerConfig(context.Background(), configCommit{cfg: &newCfg, sequence: 1}) {
		t.Fatal("routing reconfigure failed")
	}
	if service.coreManager.Selector() != oldSelector {
		t.Fatal("hot reload reset live selector")
	}
	if service.affinityPath != service.cfg.Routing.SessionAffinityStatePath {
		t.Fatal("invalid hot-reload path replaced working destination")
	}
}

func TestServiceAffinityRunRestoresBeforeAdmissionAndFinalFlushAfterDrain(t *testing.T) {
	seed, fileStore, _, opts := serviceAffinityFixture(t)
	snapshot, err := seed.coreManager.SnapshotAffinity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fileStore.Save(snapshot); err != nil {
		t.Fatal(err)
	}

	// Updater traffic is confined to a denying local proxy; no provider can be contacted.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer proxy.Close()
	cfg := *seed.cfg
	cfg.SDKConfig.ProxyURL = proxy.URL
	cfg.Models = registry.CatalogSources{Catalog: "embed", CodexCatalog: "embed", DevinCatalog: "embed"}
	cfg.Host = "127.0.0.1"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	manager := coreauth.NewManager(&affinityLifecycleStore{auths: seed.coreManager.List()}, coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{TTL: time.Hour, Fallback: &coreauth.RoundRobinSelector{}, RebindOn: "quota"}), nil)
	executor := &affinityLifecycleExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	started, idleEntered := make(chan struct{}), make(chan struct{})
	beforeStart := make(chan error, 1)
	executionErr := make(chan error, 1)
	service, err := NewBuilder().WithConfig(&cfg).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).WithCoreAuthManager(manager).
		WithWatcherFactory(func(string, string, func(*config.Config)) (*WatcherWrapper, error) { return &WatcherWrapper{}, nil }).
		WithHooks(Hooks{OnBeforeStart: func(*config.Config) {
			loaded, err := manager.SnapshotAffinity(time.Now())
			if err == nil && (len(loaded.Bindings) != 1 || loaded.Bindings[0].AuthID != "synthetic-a") {
				err = errors.New("startup lost affinity before admission")
			}
			beforeStart <- err
			manager.RegisterExecutor(executor)
			for _, auth := range manager.List() {
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "affinity-test", []*registry.ModelInfo{{ID: "synthetic-model"}})
			}
		}, OnAfterStart: func(*Service) { close(started) }}).
		WithServerOptions(api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			engine.GET("/synthetic", func(c *gin.Context) {
				response, err := manager.Execute(c.Request.Context(), []string{"affinity-test"}, cliproxyexecutor.Request{Model: "synthetic-model"}, opts)
				if err != nil {
					executionErr <- err
					c.Status(http.StatusBadGateway)
					return
				}
				c.Data(http.StatusOK, "text/plain", response.Payload)
			})
			engine.GET("/synthetic-idle", func(c *gin.Context) {
				conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				close(idleEntered)
				_, _, _ = conn.ReadMessage()
			})
		})).Build()
	if err != nil {
		t.Fatal(err)
	}
	service.pluginHost = nil
	t.Cleanup(func() {
		for _, auth := range manager.List() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		}
		if selector, ok := manager.Selector().(coreauth.StoppableSelector); ok {
			selector.Stop()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("service did not start")
	}
	if err := <-beforeStart; err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	idle, _, err := websocket.DefaultDialer.Dial("ws://"+base+"/synthetic-idle", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	<-idleEntered
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + base + "/synthetic")
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = errors.New("synthetic execution failed")
			}
		}
		requestDone <- err
	}()
	select {
	case <-executor.entered:
	case err := <-requestDone:
		select {
		case managerErr := <-executionErr:
			var authErr *coreauth.Error
			if errors.As(managerErr, &authErr) {
				t.Fatalf("synthetic manager failure code=%s", authErr.Code)
			}
			t.Fatalf("synthetic manager failure type=%T", managerErr)
		default:
			t.Fatalf("restored account did not reach synthetic executor: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic request did not start")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- service.Shutdown(shutdownCtx) }()
	for {
		_, err := manager.Execute(context.Background(), nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
		var draining *coreauth.Error
		if errors.As(err, &draining) && draining.Code == "service_draining" {
			break
		}
		select {
		case <-shutdownCtx.Done():
			t.Fatal("shutdown did not stop execution admission")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown completed before final failover result")
	default:
	}
	close(executor.release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	final, err := fileStore.Load()
	if err != nil || len(final.Bindings) != 1 || final.Bindings[0].AuthID != "synthetic-b" {
		t.Fatalf("final failover missing after drain: %v", err)
	}
}
