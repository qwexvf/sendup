package command

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"sendup/internal/cf"
	"sendup/internal/config"
)

const (
	wranglerFile     = "wrangler.jsonc"
	wranglerTemplate = "wrangler.template.jsonc"
)

// Init provisions the KV namespace and wildcard DNS record, then writes the local
// config and a wrangler.jsonc pointed at this account.
func Init(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	domain := cfg.Domain
	nsTitle := "sendup-sites"
	skipDNS := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--token":
			i++
			if i < len(args) {
				cfg.APIToken = args[i]
			}
		case "--domain":
			i++
			if i < len(args) {
				domain = args[i]
			}
		case "--namespace":
			i++
			if i < len(args) {
				nsTitle = args[i]
			}
		case "--skip-dns":
			skipDNS = true
		default:
			return fmt.Errorf("unknown argument %s", args[i])
		}
	}
	if domain == "" {
		return fmt.Errorf("which domain? pass --domain example.com (it must already be a zone on your Cloudflare account)")
	}
	cfg.Domain = domain

	if cfg.APIToken == "" {
		return fmt.Errorf("no API token. Create one at https://dash.cloudflare.com/profile/api-tokens with:\n"+
			"  Account > Workers KV Storage > Edit\n"+
			"  Account > Workers Scripts    > Edit\n"+
			"  Zone    > Workers Routes     > Edit\n"+
			"  Zone    > Zone               > Read\n"+
			"  Zone    > DNS                > Edit   (optional, see --skip-dns)\n"+
			"then: sendup init --domain %s --token <TOKEN>", domain)
	}
	api := cf.New(cfg)

	// zone + account
	resp, err := api.Do(http.MethodGet, "/zones?name="+url.QueryEscape(cfg.Domain), nil)
	if err != nil {
		return err
	}
	var zones []struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(resp.Result, &zones); err != nil {
		return fmt.Errorf("unexpected zone listing: %w", err)
	}
	if len(zones) == 0 {
		return fmt.Errorf("zone %s not found on this account", cfg.Domain)
	}
	cfg.ZoneID = zones[0].ID
	cfg.AccountID = zones[0].Account.ID
	fmt.Printf("zone     %s %s\n", cfg.Domain, cfg.ZoneID)
	fmt.Printf("account  %s\n", cfg.AccountID)
	if err := cfg.Save(); err != nil { // checkpoint — a later step failing shouldn't lose this
		return err
	}

	if err := ensureNamespace(cfg, api, nsTitle); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := ensureDNS(cfg, api, skipDNS); err != nil {
		return err
	}

	fmt.Printf("config   %s\n", config.Path())
	writeWrangler(cfg)
	fmt.Printf("\nnext:\n  wrangler deploy   (needs Workers Scripts + Workers Routes on the token)\n")
	return nil
}

func ensureNamespace(cfg *config.Config, api *cf.Client, nsTitle string) error {
	resp, err := api.Do(http.MethodGet,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces?per_page=100", cfg.AccountID), nil)
	if err != nil {
		return err
	}
	var namespaces []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(resp.Result, &namespaces); err != nil {
		return fmt.Errorf("unexpected namespace listing: %w", err)
	}

	// An id already in the config wins over a title match. Titles drift — renaming the
	// tool once pointed a re-init at a brand new, empty namespace while the real data sat
	// in the old one. The id is the thing that must not change.
	for _, n := range namespaces {
		if n.ID == cfg.NamespaceID {
			fmt.Printf("kv       %s %s (existing)\n", n.Title, n.ID)
			return nil
		}
	}
	for _, n := range namespaces {
		if n.Title == nsTitle {
			cfg.NamespaceID = n.ID
			fmt.Printf("kv       %s %s (existing)\n", nsTitle, n.ID)
			return nil
		}
	}

	body, _ := json.Marshal(map[string]string{"title": nsTitle})
	resp, err = api.Do(http.MethodPost,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces", cfg.AccountID), body)
	if err != nil {
		return err
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Result, &made); err != nil {
		return fmt.Errorf("unexpected namespace response: %w", err)
	}
	cfg.NamespaceID = made.ID
	fmt.Printf("kv       %s %s (created)\n", nsTitle, made.ID)
	return nil
}

// AAAA 100:: proxied is the documented placeholder for hostnames served only by a Worker.
func ensureDNS(cfg *config.Config, api *cf.Client, skip bool) error {
	manual := func() {
		fmt.Printf("\ndns      NOT set up — create it by hand at\n"+
			"         https://dash.cloudflare.com/?to=/:account/%s/dns\n"+
			"         Type AAAA   Name *   IPv6 100::   Proxy status: Proxied\n"+
			"         (needs Zone > DNS > Edit to automate; account-owned tokens\n"+
			"          cannot carry zone permissions at all)\n", cfg.Domain)
	}
	if skip {
		manual()
		return nil
	}

	name := "*." + cfg.Domain
	resp, err := api.Optional(http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?name=%s", cfg.ZoneID, url.QueryEscape(name)), nil)
	if err != nil {
		return err
	}
	if resp == nil {
		manual()
		return nil
	}
	var records []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp.Result, &records); err != nil {
		return fmt.Errorf("unexpected dns listing: %w", err)
	}
	if len(records) > 0 {
		fmt.Printf("dns      %s %s (existing)\n", name, records[0].Type)
		return nil
	}

	body, _ := json.Marshal(map[string]any{
		"type":    "AAAA",
		"name":    "*",
		"content": "100::",
		"proxied": true,
		"comment": "sendup — wildcard for worker route",
	})
	made, err := api.Optional(http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", cfg.ZoneID), body)
	if err != nil {
		return err
	}
	if made == nil {
		manual()
		return nil
	}
	fmt.Printf("dns      %s AAAA 100:: proxied (created)\n", name)
	return nil
}

// writeWrangler generates wrangler.jsonc from the checked-in template with this
// account's namespace and domain. The generated file is gitignored — the template in git
// stays domain-neutral.
func writeWrangler(cfg *config.Config) {
	// prefer an existing generated config so hand edits survive a re-init
	raw, err := os.ReadFile(wranglerFile)
	if err != nil {
		raw, err = os.ReadFile(wranglerTemplate)
	}
	if err != nil {
		fmt.Printf("\nwrangler no %s or %s in this directory — set these by hand before deploying:\n"+
			"           kv_namespaces[0].id = %s\n"+
			"           routes[0].pattern   = *.%s/*\n"+
			"           routes[0].zone_name = %s\n"+
			"           vars.BASE_DOMAIN    = %s\n",
			wranglerFile, wranglerTemplate, cfg.NamespaceID, cfg.Domain, cfg.Domain, cfg.Domain)
		return
	}

	out := string(raw)
	for _, sub := range []struct{ re, repl string }{
		{`"id":\s*"[^"]*"`, `"id": "` + cfg.NamespaceID + `"`},
		{`"pattern":\s*"[^"]*"`, `"pattern": "*.` + cfg.Domain + `/*"`},
		{`"zone_name":\s*"[^"]*"`, `"zone_name": "` + cfg.Domain + `"`},
		{`"BASE_DOMAIN":\s*"[^"]*"`, `"BASE_DOMAIN": "` + cfg.Domain + `"`},
	} {
		out = regexp.MustCompile(sub.re).ReplaceAllString(out, sub.repl)
	}
	out = strings.TrimPrefix(out, templateBanner)

	if err := os.WriteFile(wranglerFile, []byte(out), 0o644); err != nil {
		fmt.Printf("wrangler cannot write %s: %v\n", wranglerFile, err)
		return
	}
	fmt.Printf("wrangler %s written for %s\n", wranglerFile, cfg.Domain)
}

const templateBanner = "// Template. `sendup init` copies this to wrangler.jsonc with your account's values\n" +
	"// filled in; wrangler.jsonc is gitignored so nobody's domain lands in the repo.\n"
