// Package command implements the sendup subcommands. Each is a func(args []string) error;
// main turns a returned error into a message and a non-zero exit.
package command

import (
	"fmt"
	"os"

	"sendup/internal/cf"
	"sendup/internal/config"
)

// session is the usual preamble: load config, check the fields a command needs, and
// hand back a ready client.
func session(need ...string) (*config.Config, *cf.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Require(need...); err != nil {
		return nil, nil, err
	}
	return cfg, cf.New(cfg), nil
}

// note writes commentary to stderr, keeping stdout to URLs alone so it stays pipeable.
func note(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

func siteURL(cfg *config.Config, slug string) string {
	return fmt.Sprintf("https://%s.%s/", slug, cfg.Domain)
}

func linkURL(cfg *config.Config, slug, token string) string {
	return fmt.Sprintf("https://%s.%s/?k=%s", slug, cfg.Domain, token)
}

// record looks up one site's stored metadata.
func record(api *cf.Client, slug string) (map[string]any, error) {
	keys, err := api.ListKeys("__index:" + slug)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.Name == "__index:"+slug {
			return k.Metadata, nil
		}
	}
	return nil, nil
}
