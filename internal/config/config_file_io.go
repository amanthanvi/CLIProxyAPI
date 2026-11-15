package config

import (
	"os"
	"sync"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Loads can hash secrets and clean legacy fields, so every native file
// transaction takes the same exclusive lock. Callers release it before applying
// the returned config to handlers, watchers, or services.
var configFileMu sync.Mutex

// Native opener seam used by the in-place publication regression tests.
var openConfigFile = os.OpenFile

// ReadConfigFile returns a complete publication from participating native writers.
// Writers in other processes are outside this process-local coordination.
func ReadConfigFile(path string) ([]byte, error) {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	return os.ReadFile(path)
}

// WriteConfig preserves mounted-file identity, existing permissions and symlink
// behavior, and syncs the completed in-place write before returning.
func WriteConfig(path string, data []byte) error {
	configFileMu.Lock()
	var warnings []configFileWarning
	defer func() {
		configFileMu.Unlock()
		emitConfigFileWarnings(warnings)
	}()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) > 0 && IsV8ConfigLayout(doc.Content[0]) {
		var err error
		data, _, err = normalizeConfigLayout(data, true, &warnings)
		if err != nil {
			return err
		}
	}
	data = NormalizeCommentIndentation(data)
	f, err := openConfigFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

type configFileWarning struct {
	migration bool
	section   string
	message   string
	fields    log.Fields
}

func recordConfigFileWarning(warnings *[]configFileWarning, fields log.Fields, message string) {
	if warnings == nil {
		log.WithFields(fields).Warn(message)
		return
	}
	*warnings = append(*warnings, configFileWarning{message: message, fields: fields})
}

func emitConfigFileWarnings(warnings []configFileWarning) {
	for _, warning := range warnings {
		if warning.migration {
			warnUnrecognizedV8Section(warning.section)
		} else {
			log.WithFields(warning.fields).Warn(warning.message)
		}
	}
}
