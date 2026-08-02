// Package config holds the CLI's on-disk state: the Cloudflare credentials in
// config.json, and a record of what this machine published where.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the account sendup talks to. It holds an API token, so both the file and
// the directory are kept private.
type Config struct {
	APIToken    string `json:"apiToken,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	ZoneID      string `json:"zoneId,omitempty"`
	NamespaceID string `json:"namespaceId,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "sendup")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sendup"
	}
	return filepath.Join(home, ".config", "sendup")
}

func Path() string { return filepath.Join(Dir(), "config.json") }

// Load reads config.json, letting the environment override the domain and token.
func Load() (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(Path())
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config at %s is not valid JSON: %w", Path(), err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read %s: %w", Path(), err)
	}
	if t := os.Getenv("CLOUDFLARE_API_TOKEN"); t != "" {
		cfg.APIToken = t
	}
	if d := os.Getenv("SENDUP_DOMAIN"); d != "" {
		cfg.Domain = d
	}
	return cfg, nil
}

func (c *Config) Save() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode config: %w", err)
	}
	if err := os.WriteFile(Path(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", Path(), err)
	}
	// MkdirAll and WriteFile both leave an existing path's mode alone, so a config that
	// was ever created loosely would stay that way. This holds an API token.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("cannot secure %s: %w", dir, err)
	}
	if err := os.Chmod(Path(), 0o600); err != nil {
		return fmt.Errorf("cannot secure %s: %w", Path(), err)
	}
	return nil
}

// Require reports the first named field that is still empty.
func (c *Config) Require(fields ...string) error {
	have := map[string]string{
		"apiToken":    c.APIToken,
		"accountId":   c.AccountID,
		"namespaceId": c.NamespaceID,
		"domain":      c.Domain,
		"zoneId":      c.ZoneID,
	}
	for _, f := range fields {
		if have[f] == "" {
			return fmt.Errorf("missing %q — run `sendup init` first (or set it in %s)", f, Path())
		}
	}
	return nil
}
