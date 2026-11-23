package cliproxy

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestBuilderBuildInjectsPluginHostScheduler(t *testing.T) {
	host := pluginhost.New()
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{AuthDir: t.TempDir()}).
		WithConfigPath(t.TempDir() + "/config.yaml").
		WithPluginHost(host).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != host {
		t.Fatalf("plugin scheduler = %p, want host %p", got, host)
	}
}

func TestBuilderIdlePluginHostAllowsPoolQuotaCertificate(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	_, err := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfg.AuthDir + "/config.yaml").
		WithCoreAuthManager(manager).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	manager.RegisterExecutor(&syncTestExecutor{})
	const model = "builder-idle-host-pool"
	ctx := coreauth.WithSkipPersist(context.Background())
	for _, id := range []string{"builder-idle-host-a", "builder-idle-host-b"} {
		if _, err := manager.Register(ctx, &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		delay := time.Hour
		manager.MarkResult(ctx, coreauth.Result{AuthID: id, Provider: "codex", Model: model, CredentialScope: true,
			RetryAfter: &delay, Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic quota"}})
	}
	_, err = manager.ExecuteStream(ctx, []string{"codex"}, executor.Request{Model: model}, executor.Options{})
	details, certified := coreauth.PoolQuotaExhaustionDetails(err)
	if !certified || details.EligibleCredentials != 2 {
		t.Fatalf("Builder's idle plugin host hid the full-pool quota certificate: err=%T details=%+v", err, details)
	}
}

func TestServiceSyncPluginRuntimeConfigInjectsPluginHostScheduler(t *testing.T) {
	host := pluginhost.New()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}

	if ok := service.syncPluginRuntimeConfig(context.Background()); !ok {
		t.Fatal("syncPluginRuntimeConfig() = false, want true")
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != host {
		t.Fatalf("plugin scheduler = %p, want host %p", got, host)
	}
}

func TestServiceSyncPluginRuntimeConfigClearsPluginSchedulerWithoutHost(t *testing.T) {
	host := pluginhost.New()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}
	service.coreManager.SetPluginScheduler(host)
	service.pluginHost = nil

	if ok := service.syncPluginRuntimeConfig(context.Background()); ok {
		t.Fatal("syncPluginRuntimeConfig() = true, want false")
	}

	got := pluginSchedulerFromManager(t, service.coreManager)
	if got != nil {
		t.Fatalf("plugin scheduler = %p, want nil", got)
	}
}

func pluginSchedulerFromManager(t *testing.T, manager *coreauth.Manager) *pluginhost.Host {
	t.Helper()
	if manager == nil {
		t.Fatal("manager = nil")
	}
	value := reflect.ValueOf(manager).Elem().FieldByName("pluginScheduler")
	if !value.IsValid() {
		t.Fatal("pluginScheduler field not found")
	}
	scheduler := reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem().Interface()
	if scheduler == nil {
		return nil
	}
	host, ok := scheduler.(*pluginhost.Host)
	if !ok {
		t.Fatalf("pluginScheduler type = %T, want *pluginhost.Host", scheduler)
	}
	return host
}
