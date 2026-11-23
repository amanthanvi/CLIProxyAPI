package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

const configFileIOFixture = "port: 8317\nserver: {server-id: " + testServerID + "}\nremote-management:\n  secret-key: '$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy'\napi-keys: [synthetic-client]\nopaque-unique: retained-file-io-value\n"

func writeConfigFileIOFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func awaitConfigFileIO[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("config file transaction did not finish")
		var zero T
		return zero
	}
}

func TestConfigFileIORequiredEmptyRejectedOptionalStandbyPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, data := range []string{"", " \t\r\n"} {
		writeConfigFileIOFixture(t, path, []byte(data))
		if cfg, err := LoadConfig(path); err == nil || cfg != nil {
			t.Fatal("required empty config produced a successful default snapshot")
		}
		if cfg, err := LoadConfigOptional(path, true); err != nil || cfg == nil {
			t.Fatal("optional empty config no longer supports standby")
		}
	}
	if cfg, err := LoadConfigOptional(filepath.Join(t.TempDir(), "missing.yaml"), true); err != nil || cfg == nil {
		t.Fatal("optional missing config no longer supports standby")
	}
	writeConfigFileIOFixture(t, path, []byte("port: 8317\nremote-management:\n  secret-key: ''\n"))
	if cfg, err := LoadConfig(path); err != nil || cfg == nil || cfg.RemoteManagement.SecretKey != "" {
		t.Fatal("valid nonempty config cannot intentionally remove the management secret")
	}
}

func TestConfigFileIOTransactionsSerializeWithIncompletePublication(t *testing.T) {
	data := []byte(configFileIOFixture)
	for _, operation := range []string{"load", "optional-load", "read", "prepare", "save", "nested-scalar"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			original := []byte(strings.Replace(string(data), "port: 8317", "port: 8318", 1) + "# original tail\n")
			writeConfigFileIOFixture(t, path, original)
			cfg, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			partial, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			previousTruncate := truncateConfigFile
			var paused bool
			truncateConfigFile = func(file *os.File, size int64) error {
				if !paused && file.Name() == path {
					paused = true
					close(partial)
					<-release
				}
				return previousTruncate(file, size)
			}
			t.Cleanup(func() { truncateConfigFile = previousTruncate })
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			published := make(chan error, 1)
			go func() { published <- WriteConfig(path, data) }()
			awaitConfigFileIO(t, partial)
			if current, errRead := os.ReadFile(path); errRead != nil || !bytes.Equal(current, append(bytes.Clone(data), original[len(data):]...)) {
				t.Fatal("incomplete publication did not contain new bytes and the original tail")
			}
			started, finished := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				switch operation {
				case "load", "optional-load":
					loaded, errLoad := LoadConfigOptional(path, operation == "optional-load")
					if errLoad == nil && (loaded == nil || loaded.RemoteManagement.SecretKey != cfg.RemoteManagement.SecretKey || loaded.SourceHash != ComputeSourceHash(data)) {
						errLoad = errors.New("load observed a partial/default snapshot or mismatched source hash")
					}
					finished <- errLoad
				case "read":
					current, errRead := ReadConfigFile(path)
					if errRead == nil && !bytes.Equal(current, data) {
						errRead = errors.New("guarded read observed incomplete publication")
					}
					finished <- errRead
				case "prepare":
					finished <- PrepareConfigFileForUpload(path)
				case "save":
					finished <- SaveConfigPreserveComments(path, cfg)
				case "nested-scalar":
					finished <- SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"remote-management", "secret-key"}, cfg.RemoteManagement.SecretKey)
				}
			}()
			awaitConfigFileIO(t, started)
			completedEarly := false
			select {
			case errOperation := <-finished:
				t.Errorf("operation finished against an incomplete publication: %v", errOperation)
				completedEarly = true
			case <-time.After(50 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			if errWrite := awaitConfigFileIO(t, published); errWrite != nil {
				t.Fatal(errWrite)
			}
			if completedEarly {
				return
			}
			if errOperation := awaitConfigFileIO(t, finished); errOperation != nil {
				t.Fatal(errOperation)
			}
			current, errRead := ReadConfigFile(path)
			if errRead != nil || !bytes.Contains(current, []byte("retained-file-io-value")) {
				t.Fatal("native transaction lost untouched config data")
			}
		})
	}
}

func TestConfigFileIOLoadWritebackKeepsParsedSourceIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("port: 8317\nmanagement:\n  secret-key: synthetic-current\nremote-management:\n  secret-key: synthetic-obsolete\nopaque-unique: retained-file-io-value\n")
	writeConfigFileIOFixture(t, path, data)
	type result struct {
		cfg *Config
		err error
	}
	loaded := make(chan result, 1)
	go func() { cfg, err := LoadConfig(path); loaded <- result{cfg, err} }()
	got := awaitConfigFileIO(t, loaded)
	if got.err != nil || got.cfg == nil || !looksLikeBcrypt(got.cfg.RemoteManagement.SecretKey) || got.cfg.ParsedSourceHash != ComputeSourceHash(data) {
		t.Fatal("load writeback failed or replaced the exact parsed source identity")
	}
	current, errRead := ReadConfigFile(path)
	if errRead != nil || bytes.Contains(current, []byte("synthetic-current")) || bytes.Contains(current, []byte("synthetic-obsolete")) || !bytes.Contains(current, []byte("retained-file-io-value")) {
		t.Fatal("load writeback lost unknown data or failed to clean plaintext/conflicting keys")
	}
	if got.cfg.SourceHash != ComputeSourceHash(current) || got.cfg.SourceHash == got.cfg.ParsedSourceHash {
		t.Fatal("load did not distinguish the parsed bytes from the committed rewrite")
	}
	reloaded, errReload := LoadConfig(path)
	if errReload != nil || reloaded.SourceHash != got.cfg.SourceHash || reloaded.ParsedSourceHash != "" {
		t.Fatal("unchanged reload should carry one source hash and no rewrite provenance")
	}
}

func TestConfigFileIOWritePreservesInodeModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfigFileIOFixture(t, path, []byte(configFileIOFixture))
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, []byte(configFileIOFixture+"debug: true\n")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || (runtime.GOOS != "windows" && before.Mode().Perm() != after.Mode().Perm()) {
		t.Fatal("native write replaced the existing inode or permissions")
	}
	link := filepath.Join(dir, "linked.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Skip("symlink creation is unavailable")
	}
	if err := WriteConfig(link, []byte(configFileIOFixture)); err != nil {
		t.Fatal(err)
	}
	linked, err := os.Lstat(link)
	if err != nil || linked.Mode()&os.ModeSymlink == 0 {
		t.Fatal("native write replaced the symlink")
	}
	after, err = os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("native write replaced the symlink target")
	}
}

func TestConfigFileIOMigrationWarningsRunAfterUnlock(t *testing.T) {
	for _, operation := range []string{"write", "save", "write-error"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte("config-version: 8\nserver:\n  port: 8317\nopaque-unique: retained-file-io-value\n")
			writeConfigFileIOFixture(t, path, data)
			cfg, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			v8WarnMu.RLock()
			previousWarning := v8WarnFunc
			v8WarnMu.RUnlock()
			var warnings atomic.Int32
			SetV8MigrationWarnFunc(func(_, _ string) {
				if _, errRead := ReadConfigFile(path); errRead != nil {
					t.Error("migration warning could not reenter guarded config reader")
				}
				warnings.Add(1)
			})
			t.Cleanup(func() { SetV8MigrationWarnFunc(previousWarning) })
			if operation == "write-error" {
				previousTruncate := truncateConfigFile
				var failed bool
				truncateConfigFile = func(file *os.File, size int64) error {
					if !failed {
						failed = true
						return errors.New("synthetic config truncate failure")
					}
					return previousTruncate(file, size)
				}
				t.Cleanup(func() { truncateConfigFile = previousTruncate })
			}
			done := make(chan error, 1)
			go func() {
				if operation == "save" {
					done <- SaveConfigPreserveComments(path, cfg)
				} else {
					done <- WriteConfig(path, data)
				}
			}()
			err = awaitConfigFileIO(t, done)
			if (err != nil) != (operation == "write-error") || warnings.Load() == 0 {
				t.Fatal("migration warning emission or native write error changed")
			}
			if operation == "write-error" {
				if current, errRead := ReadConfigFile(path); errRead != nil || !bytes.Equal(current, data) {
					t.Fatal("failed publication did not restore the exact original bytes")
				}
			}
		})
	}
}

type configFileIOYAMLReader struct{ path string }

func (v configFileIOYAMLReader) MarshalYAML() (any, error) {
	_, err := ReadConfigFile(v.path)
	return "retained-custom-value", err
}

type configFileIOTextReader struct{ path string }

func (v configFileIOTextReader) MarshalText() ([]byte, error) {
	_, err := ReadConfigFile(v.path)
	return []byte("retained-custom-value"), err
}

func TestConfigFileIOSDKMarshalersRunBeforeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, value := range []any{configFileIOYAMLReader{path}, configFileIOTextReader{path}} {
		writeConfigFileIOFixture(t, path, []byte(configFileIOFixture))
		cfg, err := ParseConfigBytes([]byte(configFileIOFixture))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Payload.Default = []PayloadRule{{Params: map[string]any{"file-io": value}}}
		done := make(chan error, 1)
		go func() { done <- SaveConfigPreserveComments(path, cfg) }()
		if err := awaitConfigFileIO(t, done); err != nil {
			t.Fatal(err)
		}
		current, err := ReadConfigFile(path)
		if err != nil || !strings.Contains(string(current), "retained-custom-value") {
			t.Fatal("SDK marshaler output was not preserved")
		}
	}
}

type configFileIOReentrantHook struct {
	path     string
	observed chan error
}

func (h configFileIOReentrantHook) Levels() []log.Level { return []log.Level{log.WarnLevel} }
func (h configFileIOReentrantHook) Fire(entry *log.Entry) error {
	if entry.Message != "redis-usage-queue-retention-seconds too large; clamping to 3600" {
		return nil
	}
	if entry.Data["value"] != 3601 {
		h.observed <- errors.New("retention warning lost its structured value")
		return nil
	}
	_, err := ReadConfigFile(h.path)
	h.observed <- err
	return nil
}

func TestConfigFileIORetentionDiagnosticRunsAfterUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFileIOFixture(t, path, []byte(configFileIOFixture+"redis-usage-queue-retention-seconds: 3601\n"))
	logger := log.StandardLogger()
	previousLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() { log.SetLevel(previousLevel) })
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	observed := make(chan error, 1)
	logger.AddHook(configFileIOReentrantHook{path, observed})
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks) })
	done := make(chan error, 1)
	go func() {
		cfg, err := LoadConfig(path)
		if err == nil && cfg.RedisUsageQueueRetentionSeconds != 3600 {
			err = errors.New("retention clamp changed")
		}
		done <- err
	}()
	if err := awaitConfigFileIO(t, done); err != nil {
		t.Fatal(err)
	}
	if err := awaitConfigFileIO(t, observed); err != nil {
		t.Fatal(err)
	}
}

type configFileIODiagnosticHook struct {
	path     string
	message  string
	observed chan error
}

func (h configFileIODiagnosticHook) Levels() []log.Level { return []log.Level{log.WarnLevel} }
func (h configFileIODiagnosticHook) Fire(entry *log.Entry) error {
	if strings.HasPrefix(entry.Message, h.message) {
		_, err := ReadConfigFile(h.path)
		h.observed <- err
	}
	return nil
}

func TestConfigFileIOSanitizerDiagnosticsAllowReentrantReads(t *testing.T) {
	const base = "config-version: 8\nserver:\n  port: 8317\napi-keys:\n  gemini:\n    - name: synthetic\n      keys:\n        - api-key: synthetic-client\n"
	for _, tc := range []struct{ name, extra, message string }{
		{"claude-header", "upstream:\n  claude:\n    header-defaults:\n      client-request-id: invalid-fixture\n", "unrecognized claude header-defaults client-request-id"},
		{"antigravity-pattern", "oauth:\n  providers:\n    antigravity:\n      sensitive-patterns-replace:\n        - pattern: '['\n          replacement: synthetic\n", "sensitive-patterns-replace rule dropped: invalid regex"},
		{"devin-pattern", "oauth:\n  providers:\n    devin:\n      sensitive-patterns-replace:\n        - pattern: '['\n          replacement: synthetic\n", "sensitive-patterns-replace rule dropped: invalid regex"},
		{"claude-key-pattern", "claude-api-key:\n  - api-key: synthetic-claude\n    cloak:\n      sensitive-patterns-replace:\n        - pattern: '['\n          replacement: synthetic\n", "sensitive-patterns-replace rule dropped: invalid regex"},
		{"raw-payload", "requests:\n  payload:\n    default-raw:\n      - params:\n          synthetic: invalid-json\n", "payload rule dropped: invalid raw JSON"},
		{"replace-payload", "requests:\n  payload:\n    replace:\n      - models: [{name: '*'}]\n        target: {role: user, all: true}\n        replacements:\n          - pattern: '['\n            replacement: synthetic\n", "payload.replace rule 1 replacement 1 skipped: invalid regex"},
		{"parse-retention", "observability:\n  usage:\n    redis-usage-queue-retention-seconds: 3601\n", "redis-usage-queue-retention-seconds too large; clamping to 3600"},
	} {
		for _, operation := range []string{"load", "save", "parse"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				data := []byte(base + tc.extra)
				writeConfigFileIOFixture(t, path, data)
				cfg, err := ParseConfigBytes(data)
				if err != nil {
					t.Fatal("synthetic diagnostic fixture is invalid")
				}
				logger := log.StandardLogger()
				previousLevel := log.GetLevel()
				log.SetLevel(log.WarnLevel)
				t.Cleanup(func() { log.SetLevel(previousLevel) })
				previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
				t.Cleanup(func() { logger.ReplaceHooks(previousHooks) })
				observed := make(chan error, 2)
				logger.AddHook(configFileIODiagnosticHook{path, tc.message, observed})
				done := make(chan error, 1)
				go func() {
					switch operation {
					case "load":
						_, err = LoadConfig(path)
					case "save":
						err = SaveConfigPreserveComments(path, cfg)
					case "parse":
						_, err = ParseConfigBytes(data)
					}
					done <- err
				}()
				if err := awaitConfigFileIO(t, done); err != nil {
					t.Fatal(err)
				}
				if err := awaitConfigFileIO(t, observed); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestConfigFileIOAcceptsCompleteExternalEditAndSecretRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFileIOFixture(t, path, []byte(configFileIOFixture))
	external := []byte("port: 8318\nremote-management:\n  secret-key: ''\napi-keys: [synthetic-client]\nopaque-unique: externally-updated-value\n")
	writeConfigFileIOFixture(t, path, external)
	cfg, err := LoadConfig(path)
	if err != nil || cfg.RemoteManagement.SecretKey != "" || cfg.Port != 8318 || cfg.ParsedSourceHash != ComputeSourceHash(external) {
		t.Fatal("complete external edit was replaced by an older/default snapshot")
	}
	cfg.Debug = true
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	current, err := ReadConfigFile(path)
	if err != nil || !bytes.Contains(current, []byte("externally-updated-value")) {
		t.Fatal("save lost an untouched external config value")
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.RemoteManagement.SecretKey != "" || !reloaded.Debug {
		t.Fatal("save undid intentional management secret removal")
	}
}
