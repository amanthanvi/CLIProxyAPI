package config

import (
	"errors"
	"os"

	log "github.com/sirupsen/logrus"
)

// WriteConfig publishes a native config without replacing an existing file's
// identity, mode or symlink target. V8 migration diagnostics describe the
// rendered input and may be emitted after an unsuccessful publication.
func WriteConfig(path string, data []byte) error {
	doc, err := DecodeFirstConfigDocument(data)
	if err != nil {
		return err
	}
	if IsV8ConfigLayout(doc.Content[0]) {
		normalized, _, sections, errNorm := NormalizeConfigLayoutSilent(data, true)
		if errNorm != nil {
			return errNorm
		}
		if errValidate := ValidateConfigSave(data, normalized); errValidate != nil {
			return errValidate
		}
		data = normalized
		defer WarnUnrecognizedV8Sections(sections)
	}
	err = withConfigFile(path, configWrite, func(file *lockedConfigFile) error {
		_, errWrite := file.replace(data)
		return errWrite
	})
	if errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrConfigFileChanged) {
		return CreateConfigFile(path, data)
	}
	return err
}

type configFileWarning struct {
	message string
	fields  log.Fields
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
		log.WithFields(warning.fields).Warn(warning.message)
	}
}
