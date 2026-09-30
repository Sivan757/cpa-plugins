package pluginkit

import (
	"strconv"
	"strings"
)

// Config is a deliberately small YAML view of the plugin's own configuration
// block.
//
// CPA re-serialises plugins.configs.<pluginID> before handing it to a plugin,
// and it is free to choose the indentation width and to quote keys and values
// as it does so (the EasyCLIProxyAPI control panel emits 4-space indentation
// with quoted keys, e.g. "pat": "..."). Every setting this project defines is a
// flat scalar, so the parser tracks one level of nested mapping below a bare
// parent key and tolerates any indent width plus quoting on either side.
type Config struct {
	values map[string]string
	nested map[string]map[string]string
}

// ParseConfig reads the flat YAML subset the host sends for our plugins.
func ParseConfig(raw []byte) (*Config, error) {
	cfg := &Config{
		values: map[string]string{},
		nested: map[string]map[string]string{},
	}
	if len(raw) == 0 {
		return cfg, nil
	}

	const topIndent = -1
	var (
		currentParent string
		childIndent   = topIndent
	)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		body := strings.TrimSpace(trimmed)

		key, value, found := strings.Cut(body, ":")
		if !found {
			continue
		}
		key = unquote(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if commentIndex := strings.Index(value, " #"); commentIndex >= 0 {
			value = strings.TrimSpace(value[:commentIndex])
		}

		if indent == 0 {
			if value == "" {
				// A bare key opens the nested mapping that follows it.
				currentParent = key
				childIndent = topIndent
				if cfg.nested[key] == nil {
					cfg.nested[key] = map[string]string{}
				}
				continue
			}
			currentParent = ""
			cfg.values[key] = unquote(value)
			continue
		}

		// Indented content belongs to the most recent bare key.
		if currentParent == "" {
			continue
		}
		if childIndent == topIndent {
			childIndent = indent
		}
		// A key deeper than the child level is structure we do not model; it
		// is skipped rather than mis-attributed to the parent.
		if indent == childIndent {
			cfg.nested[currentParent][key] = unquote(value)
		}
	}
	return cfg, nil
}

// unquote strips one matching layer of single or double quotes.
func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// String returns a scalar setting, or the fallback when unset or empty.
func (c *Config) String(key, fallback string) string {
	if c == nil {
		return fallback
	}
	if value, ok := c.values[key]; ok && value != "" {
		return value
	}
	// Nested lookup keeps a plugin working whether the host nests its block
	// under the plugin id or sends it flat.
	if value, ok := c.nested[key]; ok {
		if value2, okValue := value[key]; okValue && value2 != "" {
			return value2
		}
	}
	return fallback
}

// Bool returns a boolean setting. The host writes booleans as true/false.
func (c *Config) Bool(key string, fallback bool) bool {
	raw := c.String(key, "")
	if raw == "" {
		return fallback
	}
	parsed, errParse := strconv.ParseBool(strings.ToLower(raw))
	if errParse != nil {
		return fallback
	}
	return parsed
}

// Int returns an integer setting.
func (c *Config) Int(key string, fallback int) int {
	raw := c.String(key, "")
	if raw == "" {
		return fallback
	}
	parsed, errParse := strconv.Atoi(raw)
	if errParse != nil {
		return fallback
	}
	return parsed
}

// NestedString returns a value from the nested mapping under parent.
func (c *Config) NestedString(parent, key, fallback string) string {
	if c == nil {
		return fallback
	}
	child, ok := c.nested[parent]
	if !ok {
		return fallback
	}
	if value, okValue := child[key]; okValue && value != "" {
		return value
	}
	return fallback
}

// Keys lists the top-level scalar keys, for diagnostics.
func (c *Config) Keys() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.values))
	for key := range c.values {
		out = append(out, key)
	}
	return out
}
