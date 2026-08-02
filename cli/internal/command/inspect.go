package command

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sendup/internal/cf"
	"sendup/internal/config"
	"sendup/internal/site"
)

// Pull reports what changed on the server since this machine last published — renames
// or deletions done from the dashboard or another machine.
func Pull(args []string) error {
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

	only := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown flag %s", a)
		}
		if only != "" {
			return fmt.Errorf("usage: sendup pull [file|dir]")
		}
		if only, err = filepath.Abs(a); err != nil {
			return fmt.Errorf("cannot resolve %s: %w", a, err)
		}
	}

	// server truth: source path -> slug
	keys, err := api.ListKeys("__index:")
	if err != nil {
		return err
	}
	live := map[string]string{}
	for _, k := range keys {
		if p := cf.MetaString(k.Metadata, "path"); p != "" {
			live[p] = strings.TrimPrefix(k.Name, "__index:")
		}
	}

	known := config.LoadPublished()
	seen := map[string]bool{}
	var paths []string
	for _, set := range []map[string]string{known, live} {
		for p := range set {
			if !seen[p] {
				paths, seen[p] = append(paths, p), true
			}
		}
	}
	sort.Strings(paths)

	if only != "" && !seen[only] {
		return fmt.Errorf("%s has never been published from this machine, and no site records it as its source", only)
	}

	changes := 0
	for _, p := range paths {
		if only != "" && p != only {
			continue
		}
		was, hadLocal := known[p]
		now, onServer := live[p]

		switch {
		case hadLocal && onServer && was != now:
			changes++
			fmt.Printf("renamed    %s\n           %s -> %s\n", p, was, siteURL(cfg, now))
			known[p] = now
		case hadLocal && !onServer:
			changes++
			fmt.Printf("gone       %s\n           %s no longer exists\n", p, was)
			delete(known, p)
		case !hadLocal && onServer:
			changes++
			fmt.Printf("new here   %s\n           %s\n", p, siteURL(cfg, now))
			known[p] = now
		default:
			fmt.Printf("unchanged  %s\n           %s\n", p, siteURL(cfg, now))
		}
	}
	known.Save()

	if changes == 0 {
		note("\nnothing changed")
	} else {
		note("\n%d change%s, local record updated", changes, site.Plural(changes))
	}
	return nil
}

// Doctor compares local config against what Cloudflare actually has, and against the
// generated wrangler.jsonc. Drift between the three is silent otherwise — a stale
// namespace id points every command at an empty store while the real data sits elsewhere.
func Doctor(args []string) error {
	fix := false
	for _, a := range args {
		if a != "--fix" {
			return fmt.Errorf("unknown argument %s", a)
		}
		fix = true
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.APIToken == "" || cfg.AccountID == "" || cfg.NamespaceID == "" || cfg.Domain == "" {
		return fmt.Errorf("config is incomplete — run `sendup init --domain <your-domain> --token <TOKEN>`")
	}
	api := cf.New(cfg)

	problems := 0
	report := func(state, label, detail string) {
		if state == "FAIL" {
			problems++
		}
		fmt.Printf("%-5s %-22s %s\n", state, label, detail)
	}

	report("ok", "config", config.Path())
	report("ok", "domain", cfg.Domain)

	checkZone(cfg, api, report)
	checkNamespace(cfg, api, report)
	checkDNS(cfg, api, report)
	checkWrangler(cfg, fix, report)
	checkDashboard(cfg, api, report)
	checkWorker(cfg, report)

	if problems > 0 {
		note("\n%d problem%s", problems, site.Plural(problems))
		os.Exit(1)
	}
	note("\nall good")
	return nil
}

type reporter func(state, label, detail string)

func checkZone(cfg *config.Config, api *cf.Client, report reporter) {
	resp, err := api.Optional(http.MethodGet, "/zones?name="+url.QueryEscape(cfg.Domain), nil)
	if err != nil {
		report("FAIL", "zone", err.Error())
		return
	}
	if resp == nil {
		report("warn", "zone", "token cannot read zones — skipped (needs Zone > Zone > Read)")
		return
	}
	var zones []struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(resp.Result, &zones); err != nil {
		report("FAIL", "zone", "unexpected listing: "+err.Error())
		return
	}
	switch {
	case len(zones) == 0:
		report("FAIL", "zone", cfg.Domain+" is not a zone on this account any more")
	case zones[0].ID != cfg.ZoneID:
		report("FAIL", "zone", fmt.Sprintf("id changed: config %s, Cloudflare %s", cfg.ZoneID, zones[0].ID))
	case zones[0].Account.ID != cfg.AccountID:
		report("FAIL", "account", fmt.Sprintf("id changed: config %s, Cloudflare %s", cfg.AccountID, zones[0].Account.ID))
	default:
		report("ok", "zone", cfg.ZoneID)
	}
}

func checkNamespace(cfg *config.Config, api *cf.Client, report reporter) {
	resp, err := api.Optional(http.MethodGet,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces?per_page=100", cfg.AccountID), nil)
	if err != nil {
		report("FAIL", "kv namespace", err.Error())
		return
	}
	if resp == nil {
		report("warn", "kv namespace", "token cannot list namespaces — skipped")
		return
	}
	var namespaces []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(resp.Result, &namespaces); err != nil {
		report("FAIL", "kv namespace", "unexpected listing: "+err.Error())
		return
	}
	for _, n := range namespaces {
		if n.ID == cfg.NamespaceID {
			report("ok", "kv namespace", n.Title+" "+n.ID)
			return
		}
	}
	others := make([]string, 0, len(namespaces))
	for _, n := range namespaces {
		others = append(others, n.Title+" "+n.ID)
	}
	report("FAIL", "kv namespace",
		fmt.Sprintf("%s does not exist. On the account: %s", cfg.NamespaceID, strings.Join(others, ", ")))
}

func checkDNS(cfg *config.Config, api *cf.Client, report reporter) {
	name := "*." + cfg.Domain
	resp, err := api.Optional(http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?name=%s", cfg.ZoneID, url.QueryEscape(name)), nil)
	if err != nil {
		report("FAIL", "wildcard dns", err.Error())
		return
	}
	if resp == nil {
		report("warn", "wildcard dns", "token cannot read DNS — skipped")
		return
	}
	var records []struct {
		Type    string `json:"type"`
		Proxied bool   `json:"proxied"`
	}
	if err := json.Unmarshal(resp.Result, &records); err != nil {
		report("FAIL", "wildcard dns", "unexpected listing: "+err.Error())
		return
	}
	switch {
	case len(records) == 0:
		report("FAIL", "wildcard dns", name+" is missing — add AAAA * 100:: proxied")
	case !records[0].Proxied:
		report("FAIL", "wildcard dns", name+" exists but is DNS-only; it must be proxied")
	default:
		report("ok", "wildcard dns", name+" "+records[0].Type+" proxied")
	}
}

func checkWrangler(cfg *config.Config, fix bool, report reporter) {
	raw, err := os.ReadFile(wranglerFile)
	if err != nil {
		report("warn", wranglerFile, "not in this directory — run doctor from the repo root")
		return
	}
	text := string(raw)
	var drift []string
	if !strings.Contains(text, `"id": "`+cfg.NamespaceID+`"`) {
		drift = append(drift, "kv namespace id")
	}
	if !strings.Contains(text, `"BASE_DOMAIN": "`+cfg.Domain+`"`) {
		drift = append(drift, "BASE_DOMAIN")
	}
	if !strings.Contains(text, `"zone_name": "`+cfg.Domain+`"`) {
		drift = append(drift, "route zone_name")
	}
	switch {
	case len(drift) == 0:
		report("ok", wranglerFile, "matches config")
	case fix:
		writeWrangler(cfg)
		report("ok", wranglerFile, "rewrote "+strings.Join(drift, ", "))
	default:
		report("FAIL", wranglerFile, strings.Join(drift, ", ")+" disagree with config — `sendup doctor --fix`")
	}
}

func checkDashboard(cfg *config.Config, api *cf.Client, report reporter) {
	keys, err := api.ListKeys("__admin")
	if err != nil {
		report("FAIL", "dashboard", err.Error())
		return
	}
	for _, k := range keys {
		if k.Name == "__admin" {
			report("ok", "dashboard", "https://dash."+cfg.Domain+"/")
			return
		}
	}
	report("warn", "dashboard", "not set up — `sendup admin` if you want it")
}

// checkWorker asks a subdomain that cannot exist for a page, and expects the Worker's
// own 404 back. Anything else means the route is unbound or nothing is deployed.
func checkWorker(cfg *config.Config, report reporter) {
	slug, err := site.NewSlug()
	if err != nil {
		report("warn", "worker", "skipped: "+err.Error())
		return
	}
	probe := siteURL(cfg, slug)
	res, err := cf.HTTPClient().Get(probe)
	if err != nil {
		report("FAIL", "worker", "no response from "+probe+": "+err.Error())
		return
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound && strings.Contains(string(body), "no such site") {
		report("ok", "worker", "serving on *."+cfg.Domain)
		return
	}
	report("FAIL", "worker",
		fmt.Sprintf("%s answered %d, not the worker's 404 — deploy may be missing or the route unbound",
			probe, res.StatusCode))
}
