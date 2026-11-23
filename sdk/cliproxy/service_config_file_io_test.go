package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const configFileIOManagementKey = "config-file-io-management-fixture"
const configFileIOAPIKey = "config-file-io-client-fixture"

type configFileIOHarness struct {
	service    *Service
	handler    http.Handler
	configPath string
	original   []byte
	secretHash string
}

func newConfigFileIOHarness(t *testing.T) *configFileIOHarness {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	previousLevel := log.GetLevel()
	t.Cleanup(func() { log.SetLevel(previousLevel) })
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	hashedKey, errHash := bcrypt.GenerateFromPassword([]byte(configFileIOManagementKey), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	raw := []byte(fmt.Sprintf(`config-version: 8
server:
  port: 8317
  max-request-body-bytes: 12345678
oauth:
  auth-dir: %q
management:
  allow-remote: true
  secret-key: %q
access:
  api-keys: [%q]
credentials:
  concurrency:
    max-limit: 41
routing:
  retry:
    request-retry: 4
plugins:
  enabled: false
  configs:
    config-file-io-opaque:
      enabled: false
      unique: keep-this-setting
      nested: {items: [first, second], enabled: false}
      payload: %q
`, filepath.ToSlash(authDir), string(hashedKey), configFileIOAPIKey, strings.Repeat("opaque-file-io-fixture-", 1024)))
	if errValidate := internalconfig.ValidateV8Config(raw); errValidate != nil {
		t.Fatal("synthetic config is not valid v8 input")
	}
	configPath := filepath.Join(dir, "config.yaml")
	if errWrite := os.WriteFile(configPath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := internalconfig.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatal("load synthetic config failed")
	}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if errBuild != nil {
		t.Fatal("build synthetic service failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	watcherWrapper, errWatcher := service.watcherFactory(configPath, authDir, func(newCfg *config.Config) {
		service.applyWatcherConfigUpdate(newCfg)
	})
	if errWatcher != nil {
		t.Fatal("create synthetic config watcher failed")
	}
	service.setWatcher(watcherWrapper)
	service.ensureAuthUpdateQueue(ctx)
	watcherWrapper.SetAuthUpdateQueue(service.authUpdates)
	watcherWrapper.SetConfig(service.currentConfig())
	server := api.NewServer(service.currentConfig(), service.coreManager, service.accessManager, configPath, service.serverOptions...)
	if server == nil {
		t.Fatal("NewServer returned nil")
	}
	service.server = server
	t.Cleanup(func() {
		if service.authQueueStop != nil {
			service.authQueueStop()
		}
		_ = watcherWrapper.Stop()
	})
	return &configFileIOHarness{
		service: service, handler: server.Handler(), configPath: configPath,
		original: raw, secretHash: string(hashedKey),
	}
}

func (h *configFileIOHarness) request(method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/v8/management/config"+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+configFileIOManagementKey)
	request.Header.Set("Content-Type", "application/json")
	h.handler.ServeHTTP(response, request)
	return response
}

func (h *configFileIOHarness) assertRetained(t *testing.T) {
	t.Helper()
	raw, errRead := os.ReadFile(h.configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	disk, errParse := internalconfig.ParseConfigBytes(raw)
	if errParse != nil {
		t.Fatal("saved synthetic config is invalid")
	}
	for _, cfg := range []*config.Config{disk, h.service.currentConfig()} {
		if cfg == nil || cfg.RemoteManagement.SecretKey != h.secretHash || !reflect.DeepEqual(cfg.APIKeys, []string{configFileIOAPIKey}) || cfg.MaxRequestBodyBytes != 12345678 {
			t.Fatal("disk or runtime lost an untouched credential or server setting")
		}
	}
	var before, after map[string]any
	if errBefore := yaml.Unmarshal(h.original, &before); errBefore != nil {
		t.Fatal("decode original synthetic config failed")
	}
	if errAfter := yaml.Unmarshal(raw, &after); errAfter != nil {
		t.Fatal("decode saved synthetic config failed")
	}
	if !reflect.DeepEqual(before["plugins"], after["plugins"]) {
		t.Fatal("management writes lost untouched opaque YAML settings")
	}
	if response := h.request(http.MethodGet, "", ""); response.Code != http.StatusOK {
		t.Fatalf("management GET after acknowledged writes: status=%d, body-bytes=%d", response.Code, response.Body.Len())
	}
}

func TestConfigFileIOConcurrentManagementPUTPATCHRetainsConfig(t *testing.T) {
	h := newConfigFileIOHarness(t)
	const iterations = 64
	start := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		<-start
		for iteration := 0; iteration < iterations; iteration++ {
			body := fmt.Sprintf(`{"max-limit":%d}`, 100+iteration)
			response := h.request(http.MethodPut, "/credentials/concurrency", body)
			if response.Code != http.StatusOK {
				t.Errorf("concurrent management PUT iteration=%d: status=%d, body-bytes=%d", iteration, response.Code, response.Body.Len())
				return
			}
		}
	}()
	go func() {
		defer writers.Done()
		<-start
		for iteration := 0; iteration < iterations; iteration++ {
			body := fmt.Sprintf(`{"routing":{"retry":{"request-retry":%d}}}`, 10+iteration)
			response := h.request(http.MethodPatch, "", body)
			if response.Code != http.StatusOK {
				t.Errorf("concurrent management PATCH iteration=%d: status=%d, body-bytes=%d", iteration, response.Code, response.Body.Len())
				return
			}
		}
	}()
	// The request barrier starts real route writers together; it does not force
	// any particular native read/truncate interleaving.
	close(start)
	writers.Wait()
	h.service.reloadConfigFromWatcher()
	h.assertRetained(t)
	for _, cfg := range []*config.Config{mustLoadConfigFileIO(t, h.configPath), h.service.currentConfig()} {
		if cfg.CredentialConcurrency.MaxLimit != 100+iterations-1 || cfg.RequestRetry != 10+iterations-1 {
			t.Fatal("disk or runtime lost an acknowledged concurrent management mutation")
		}
	}
}

func mustLoadConfigFileIO(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, errLoad := internalconfig.LoadConfig(path)
	if errLoad != nil {
		t.Fatal("reload saved synthetic config failed")
	}
	return cfg
}

func TestConfigFileIOValidSecretRemovalDisablesAndRestorationEnablesManagement(t *testing.T) {
	h := newConfigFileIOHarness(t)
	response := h.request(http.MethodPatch, "/management", `{"secret-key":""}`)
	if response.Code != http.StatusOK {
		t.Fatalf("intentional secret removal: status=%d, body-bytes=%d", response.Code, response.Body.Len())
	}
	h.service.reloadConfigFromWatcher()
	disk := mustLoadConfigFileIO(t, h.configPath)
	if disk.RemoteManagement.SecretKey != "" || h.service.currentConfig().RemoteManagement.SecretKey != "" {
		t.Fatal("valid nonempty config did not remove the management secret")
	}
	response = h.request(http.MethodGet, "", "")
	if response.Code != http.StatusNotFound || response.Body.Len() != 0 {
		t.Fatalf("management without a secret: status=%d, body-bytes=%d, want blank 404", response.Code, response.Body.Len())
	}
	if errRestore := management.WriteConfig(h.configPath, h.original); errRestore != nil {
		t.Fatal("restore valid synthetic config failed")
	}
	h.service.reloadConfigFromWatcher()
	h.assertRetained(t)
}
