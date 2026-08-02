package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// The authoritative source-path -> slug mapping lives in KV, so publishing always finds
// the right site even if someone renamed it elsewhere. This file exists only so
// `sendup pull` can tell you *that it changed* — without a local last-known value there
// is nothing to compare against. It stores slugs, never content.

type Published map[string]string // absolute source path -> slug

func PublishedPath() string { return filepath.Join(Dir(), "published.json") }

func LoadPublished() Published {
	p := Published{}
	data, err := os.ReadFile(PublishedPath())
	if err != nil {
		return p
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Published{} // a corrupt record is not worth failing a publish over
	}
	return p
}

// Save is best-effort: losing this record costs a `pull` diff, nothing more.
func (p Published) Save() {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(PublishedPath(), append(data, '\n'), 0o600)
	_ = os.Chmod(PublishedPath(), 0o600)
}

func Remember(absPath, slug string) {
	p := LoadPublished()
	p[absPath] = slug
	p.Save()
}
