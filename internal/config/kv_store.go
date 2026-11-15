package config

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"gopkg.in/yaml.v3"
)

// Enabled requires explicit disabled:false or a path, never the zero-value config.
func (c KVStoreConfig) Enabled() bool {
	return !c.Disabled && (c.disabledSet || strings.TrimSpace(c.Path) != "")
}

// UnmarshalYAML preserves explicit opt-in without activating empty sections.
func (c *KVStoreConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain KVStoreConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = KVStoreConfig(decoded)
	disabled := yamlPath(node, "disabled")
	c.disabledSet = disabled != nil && disabled.Tag != "!!null"
	if !c.disabledSet && strings.TrimSpace(c.Path) == "" {
		c.Disabled = true
	}
	return nil
}

// MarshalYAML retains the effective disabled state across config round trips.
func (c KVStoreConfig) MarshalYAML() (any, error) {
	type plain KVStoreConfig
	encoded := plain(c)
	encoded.Disabled = !c.Enabled()
	return encoded, nil
}

func (c *KVStoreConfig) UnmarshalJSON(data []byte) error {
	if !ojson.ValidBytes(data) {
		return fmt.Errorf("kv-store must be valid JSON")
	}
	root := ojson.ParseBytes(data)
	if root.Type == ojson.Null {
		*c = KVStoreConfig{Disabled: true}
		return nil
	}
	if !root.IsObject() {
		return fmt.Errorf("kv-store must be an object")
	}
	next := KVStoreConfig{}
	if disabled := root.Get("disabled"); disabled.Exists() && disabled.Type != ojson.Null {
		if disabled.Type != ojson.True && disabled.Type != ojson.False {
			return fmt.Errorf("kv-store.disabled must be a boolean")
		}
		next.Disabled = disabled.Bool()
		next.disabledSet = true
	}
	if path := root.Get("path"); path.Exists() && path.Type != ojson.Null {
		if path.Type != ojson.String {
			return fmt.Errorf("kv-store.path must be a string")
		}
		next.Path = path.String()
	}
	if !next.disabledSet && strings.TrimSpace(next.Path) == "" {
		next.Disabled = true
	}
	*c = next
	return nil
}

func (c KVStoreConfig) MarshalJSON() ([]byte, error) {
	doc := ojson.NewDocString(`{}`)
	if err := doc.Set("disabled", !c.Enabled()); err != nil {
		return nil, err
	}
	if c.Path != "" {
		if err := doc.Set("path", c.Path); err != nil {
			return nil, err
		}
	}
	return doc.Bytes(), nil
}
