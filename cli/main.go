// sendup — publish static pages to a subdomain of a domain you control
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase = "https://api.cloudflare.com/client/v4"

	maxValue     = 25 << 20 // KV per-value cap
	maxBulkBytes = 90 << 20 // API cap is 100MB, leave headroom
	maxBulkKeys  = 10000
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sendup: "+format+"\n", a...)
	os.Exit(1)
}

// ---------- config ----------

type Config struct {
	APIToken    string `json:"apiToken,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	ZoneID      string `json:"zoneId,omitempty"`
	NamespaceID string `json:"namespaceId,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "sendup")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		die("cannot locate home directory: %v", err)
	}
	return filepath.Join(home, ".config", "sendup")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

func loadConfig() *Config {
	cfg := &Config{}
	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			die("config at %s is not valid JSON: %v", configPath(), err)
		}
	} else if !os.IsNotExist(err) {
		die("cannot read %s: %v", configPath(), err)
	}
	if t := os.Getenv("CLOUDFLARE_API_TOKEN"); t != "" {
		cfg.APIToken = t
	}
	if d := os.Getenv("SENDUP_DOMAIN"); d != "" {
		cfg.Domain = d
	}
	return cfg
}

func (c *Config) save() {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		die("cannot create %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		die("cannot encode config: %v", err)
	}
	if err := os.WriteFile(configPath(), append(data, '\n'), 0o600); err != nil {
		die("cannot write %s: %v", configPath(), err)
	}
	// MkdirAll and WriteFile both leave an existing path's mode alone, so a config that
	// was ever created loosely would stay that way. This holds an API token.
	if err := os.Chmod(dir, 0o700); err != nil {
		die("cannot secure %s: %v", dir, err)
	}
	if err := os.Chmod(configPath(), 0o600); err != nil {
		die("cannot secure %s: %v", configPath(), err)
	}
}

func (c *Config) require(fields ...string) {
	missing := map[string]string{
		"apiToken":    c.APIToken,
		"accountId":   c.AccountID,
		"namespaceId": c.NamespaceID,
		"domain":      c.Domain,
		"zoneId":      c.ZoneID,
	}
	for _, f := range fields {
		if missing[f] == "" {
			die("missing %q — run `sendup init` first (or set it in %s)", f, configPath())
		}
	}
}

// ---------- local record of what went where ----------
//
// The authoritative source-path -> slug mapping lives in KV, so publishing always finds
// the right site even if someone renamed it elsewhere. This file exists only so we can
// tell you *that it changed* — without a local last-known value there is nothing to
// compare against.

type published map[string]string // absolute source path -> slug

func publishedPath() string { return filepath.Join(configDir(), "published.json") }

func loadPublished() published {
	p := published{}
	data, err := os.ReadFile(publishedPath())
	if err != nil {
		return p
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return published{} // a corrupt cache is not worth failing a publish over
	}
	return p
}

func (p published) save() {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(publishedPath(), append(data, '\n'), 0o600)
	_ = os.Chmod(publishedPath(), 0o600)
}

func rememberPublished(absPath, slug string) {
	p := loadPublished()
	p[absPath] = slug
	p.save()
}

// ---------- cloudflare api ----------

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfResp struct {
	Success    bool            `json:"success"`
	Errors     []cfError       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Cursor string `json:"cursor"`
	} `json:"result_info"`
}

func permissionFor(path string) string {
	switch {
	case strings.Contains(path, "/dns_records"):
		return "Zone > DNS > Edit"
	case strings.Contains(path, "/storage/kv/"):
		return "Account > Workers KV Storage > Edit"
	case strings.HasPrefix(path, "/zones?"):
		return "Zone > Zone > Read"
	}
	return "a broader scope"
}

// cf returns (nil, nil) instead of exiting when tolerate403 is set and the token
// lacks the permission for this call.
func cf(cfg *Config, method, path string, body []byte, tolerate403 bool) *cfResp {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, apiBase+path, rdr)
	if err != nil {
		die("bad request for %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		die("request to %s failed: %v", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		die("reading response from %s failed: %v", path, err)
	}

	var out cfResp
	if err := json.Unmarshal(raw, &out); err != nil {
		die("API %d on %s: %s", res.StatusCode, path, truncate(string(raw), 300))
	}
	if out.Success {
		return &out
	}

	authFail := res.StatusCode == http.StatusForbidden
	msgs := make([]string, 0, len(out.Errors))
	for _, e := range out.Errors {
		if e.Code == 10000 {
			authFail = true
		}
		msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	if tolerate403 && authFail {
		return nil
	}

	detail := strings.Join(msgs, "; ")
	if detail == "" {
		detail = truncate(string(raw), 300)
	}
	hint := ""
	if authFail {
		hint = fmt.Sprintf("\n  the token is missing a permission for this call — it needs %s."+
			"\n  edit it at https://dash.cloudflare.com/profile/api-tokens, then re-run the same command.",
			permissionFor(path))
	}
	die("API %d on %s: %s%s", res.StatusCode, path, detail, hint)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------- helpers ----------

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)

// dash.<domain> is the dashboard; www/api are the names people reach for by reflex
var reserved = map[string]bool{"dash": true, "www": true, "api": true}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// pageTitle pulls <title> out of an HTML document for the dashboard listing.
func pageTitle(buf []byte) string {
	if len(buf) > 64<<10 {
		buf = buf[:64<<10] // the head is at the front; don't scan a huge document
	}
	m := titleRe.FindSubmatch(buf)
	if m == nil {
		return ""
	}
	t := strings.TrimSpace(html.UnescapeString(string(m[1])))
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > 120 {
		t = t[:120]
	}
	return t
}

func newSlug() string {
	var b [8]byte // 64 bits
	if _, err := rand.Read(b[:]); err != nil {
		die("cannot generate slug: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func newPassword() string {
	var b [12]byte // 96 bits
	if _, err := rand.Read(b[:]); err != nil {
		die("cannot generate password: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func newSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		die("cannot generate secret: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// mintToken builds "<exp>.<sig>" where exp is unix seconds (0 = never) and sig is the
// first 16 bytes of HMAC-SHA256(secret, "<slug>:<exp>"). The expiry sits inside the
// signed message, so a recipient cannot extend their own link.
func mintToken(secretHex, slug string, exp int64) string {
	key, err := hex.DecodeString(secretHex)
	if err != nil {
		die("stored link secret is corrupt")
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s:%d", slug, exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(mac.Sum(nil)[:16]))
}

func linkURL(cfg *Config, slug, token string) string {
	return fmt.Sprintf("https://%s.%s/?k=%s", slug, cfg.Domain, token)
}

// hashCreds returns a random salt and sha256(salt + "user:pass"), both hex.
// The worker recomputes this from the Basic auth header and compares in constant time.
func hashCreds(user, pass string) (salt, hash string) {
	var s [16]byte
	if _, err := rand.Read(s[:]); err != nil {
		die("cannot generate salt: %v", err)
	}
	salt = hex.EncodeToString(s[:])
	sum := sha256.Sum256([]byte(salt + user + ":" + pass))
	return salt, hex.EncodeToString(sum[:])
}

type upload struct {
	path string // site-absolute, e.g. /index.html
	abs  string
	size int64
}

func collectFiles(target string) []upload {
	st, err := os.Stat(target)
	if err != nil {
		die("no such path: %s", target)
	}

	if !st.IsDir() {
		abs, _ := filepath.Abs(target)
		// a lone page always becomes the site root
		p := "/" + filepath.Base(target)
		switch strings.ToLower(filepath.Ext(target)) {
		case ".html", ".htm", ".md":
			p = "/index.html"
		}
		return []upload{{path: p, abs: abs, size: st.Size()}}
	}

	root, _ := filepath.Abs(target)
	var out []upload
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, upload{path: "/" + filepath.ToSlash(rel), abs: p, size: info.Size()})
		return nil
	})
	if err != nil {
		die("walking %s: %v", target, err)
	}
	if len(out) == 0 {
		die("no files under %s", target)
	}

	hasIndex := false
	for _, f := range out {
		if f.path == "/index.html" {
			hasIndex = true
		}
	}
	if !hasIndex {
		fmt.Fprintln(os.Stderr, `sendup: warning — no index.html at the root, "/" will 404`)
	}
	return out
}

func fmtBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

type kvPair struct {
	Key           string         `json:"key"`
	Value         string         `json:"value"`
	Base64        bool           `json:"base64,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	ExpirationTTL int64          `json:"expiration_ttl,omitempty"`
}

// bulkPut chunks so no single request blows the 100MB / 10k-key ceiling.
func bulkPut(cfg *Config, pairs []kvPair) {
	path := fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/bulk", cfg.AccountID, cfg.NamespaceID)
	batch := make([]kvPair, 0, len(pairs))
	var size int

	flush := func() {
		if len(batch) == 0 {
			return
		}
		body, err := json.Marshal(batch)
		if err != nil {
			die("cannot encode upload: %v", err)
		}
		cf(cfg, http.MethodPut, path, body, false)
		batch = batch[:0]
		size = 0
	}

	for _, p := range pairs {
		if len(batch) >= maxBulkKeys || size+len(p.Value) > maxBulkBytes {
			flush()
		}
		batch = append(batch, p)
		size += len(p.Value)
	}
	flush()
}

type kvKey struct {
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata"`
}

func listKeys(cfg *Config, prefix string) []kvKey {
	var keys []kvKey
	cursor := ""
	for {
		q := url.Values{}
		q.Set("prefix", prefix)
		q.Set("limit", "1000")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		path := fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/keys?%s",
			cfg.AccountID, cfg.NamespaceID, q.Encode())
		resp := cf(cfg, http.MethodGet, path, nil, false)

		var page []kvKey
		if err := json.Unmarshal(resp.Result, &page); err != nil {
			die("unexpected key listing: %v", err)
		}
		keys = append(keys, page...)
		cursor = resp.ResultInfo.Cursor
		if cursor == "" {
			return keys
		}
	}
}

func metaString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func metaNumber(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}

// ---------- commands ----------

func cmdInit(args []string) {
	cfg := loadConfig()
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
			die("unknown argument %s", args[i])
		}
	}
	if domain == "" {
		die("which domain? pass --domain example.com (it must already be a zone on your Cloudflare account)")
	}
	cfg.Domain = domain

	if cfg.APIToken == "" {
		die("no API token. Create one at https://dash.cloudflare.com/profile/api-tokens with:\n" +
			"  Account > Workers KV Storage > Edit\n" +
			"  Account > Workers Scripts    > Edit\n" +
			"  Zone    > Workers Routes     > Edit\n" +
			"  Zone    > Zone               > Read\n" +
			"  Zone    > DNS                > Edit   (optional, see --skip-dns)\n" +
			"then: sendup init --token <TOKEN>   (or export CLOUDFLARE_API_TOKEN=...)")
	}

	// zone + account
	resp := cf(cfg, http.MethodGet, "/zones?name="+url.QueryEscape(cfg.Domain), nil, false)
	var zones []struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(resp.Result, &zones); err != nil {
		die("unexpected zone listing: %v", err)
	}
	if len(zones) == 0 {
		die("zone %s not found on this account", cfg.Domain)
	}
	cfg.ZoneID = zones[0].ID
	cfg.AccountID = zones[0].Account.ID
	fmt.Printf("zone     %s %s\n", cfg.Domain, cfg.ZoneID)
	fmt.Printf("account  %s\n", cfg.AccountID)
	cfg.save() // checkpoint — a later step failing shouldn't lose this

	// kv namespace (reuse if it already exists)
	resp = cf(cfg, http.MethodGet,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces?per_page=100", cfg.AccountID), nil, false)
	var namespaces []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(resp.Result, &namespaces); err != nil {
		die("unexpected namespace listing: %v", err)
	}
	found := ""
	// An id already in the config wins over a title match. Titles drift — renaming the
	// tool once pointed a re-init at a brand new, empty namespace while the real data sat
	// in the old one. The id is the thing that must not change.
	for _, n := range namespaces {
		if n.ID == cfg.NamespaceID {
			found = n.ID
			nsTitle = n.Title
		}
	}
	for _, n := range namespaces {
		if found == "" && n.Title == nsTitle {
			found = n.ID
		}
	}
	if found != "" {
		cfg.NamespaceID = found
		fmt.Printf("kv       %s %s (existing)\n", nsTitle, cfg.NamespaceID)
	} else {
		body, _ := json.Marshal(map[string]string{"title": nsTitle})
		resp = cf(cfg, http.MethodPost,
			fmt.Sprintf("/accounts/%s/storage/kv/namespaces", cfg.AccountID), body, false)
		var made struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(resp.Result, &made); err != nil {
			die("unexpected namespace response: %v", err)
		}
		cfg.NamespaceID = made.ID
		fmt.Printf("kv       %s %s (created)\n", nsTitle, cfg.NamespaceID)
	}
	cfg.save()

	initDNS(cfg, skipDNS)

	fmt.Printf("config   %s\n", configPath())
	patchWrangler(cfg)
	fmt.Printf("\nnext:\n  wrangler deploy   (needs Workers Scripts + Workers Routes on the token)\n")
}

// Generate wrangler.jsonc from the checked-in template with this account's namespace and
// domain, so `wrangler deploy` works straight after a clone. The generated file is
// gitignored — the template in git stays domain-neutral.
func patchWrangler(cfg *Config) {
	const name = "wrangler.jsonc"
	const tmpl = "wrangler.template.jsonc"

	// prefer an existing generated config so hand edits survive a re-init
	raw, err := os.ReadFile(name)
	if err != nil {
		raw, err = os.ReadFile(tmpl)
	}
	if err != nil {
		fmt.Printf("\nwrangler no %s or %s in this directory — set these by hand before deploying:\n"+
			"           kv_namespaces[0].id = %s\n"+
			"           routes[0].pattern   = *.%s/*\n"+
			"           routes[0].zone_name = %s\n"+
			"           vars.BASE_DOMAIN    = %s\n",
			name, tmpl, cfg.NamespaceID, cfg.Domain, cfg.Domain, cfg.Domain)
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

	// strip the template's leading explainer if we are generating from it
	out = strings.TrimPrefix(out, "// Template. `sendup init` copies this to wrangler.jsonc with your account's values\n// filled in; wrangler.jsonc is gitignored so nobody's domain lands in the repo.\n")
	if err := os.WriteFile(name, []byte(out), 0o644); err != nil {
		die("cannot write %s: %v", name, err)
	}
	fmt.Printf("wrangler %s written for %s\n", name, cfg.Domain)
}

// AAAA 100:: proxied is the documented placeholder for hostnames served only by a Worker.
func initDNS(cfg *Config, skip bool) {
	manual := func() {
		fmt.Printf("\ndns      NOT set up — create it by hand at\n"+
			"         https://dash.cloudflare.com/?to=/:account/%s/dns\n"+
			"         Type AAAA   Name *   IPv6 100::   Proxy status: Proxied\n"+
			"         (needs Zone > DNS > Edit to automate; account-owned tokens\n"+
			"          cannot carry zone permissions at all)\n", cfg.Domain)
	}
	if skip {
		manual()
		return
	}

	name := "*." + cfg.Domain
	resp := cf(cfg, http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?name=%s", cfg.ZoneID, url.QueryEscape(name)), nil, true)
	if resp == nil {
		manual()
		return
	}
	var records []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp.Result, &records); err != nil {
		die("unexpected dns listing: %v", err)
	}
	if len(records) > 0 {
		fmt.Printf("dns      %s %s (existing)\n", name, records[0].Type)
		return
	}

	body, _ := json.Marshal(map[string]any{
		"type":    "AAAA",
		"name":    "*",
		"content": "100::",
		"proxied": true,
		"comment": "sendup — wildcard for worker route",
	})
	if cf(cfg, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", cfg.ZoneID), body, true) == nil {
		manual()
		return
	}
	fmt.Printf("dns      %s AAAA 100:: proxied (created)\n", name)
}

func cmdPublish(args []string) {
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	target, slug := "", ""
	ttlDays := 0.0
	wantAuth, noAuth, forceNew := false, false, false
	wantLink, noLink, rotate := false, false, false
	linkTTLDays := 0.0
	authUser, authPass := "", ""

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--slug", a == "--name":
			i++
			if i < len(args) {
				slug = args[i]
			}
		case a == "--auth":
			wantAuth = true
			// optional "user:pass"; bare --auth generates both
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				u, p, ok := strings.Cut(args[i], ":")
				if !ok || u == "" || p == "" {
					die("--auth wants user:pass (or bare --auth to generate credentials)")
				}
				authUser, authPass = u, p
			}
		case a == "--no-auth":
			noAuth = true
		case a == "--link":
			wantLink = true
		case a == "--no-link":
			noLink = true
		case a == "--rotate":
			rotate = true
		case a == "--link-ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					die("--link-ttl wants a positive number of days")
				}
				linkTTLDays, wantLink = v, true
			}
		case a == "--new":
			forceNew = true
		case a == "--ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					die("--ttl wants a positive number of days")
				}
				ttlDays = v
			}
		case strings.HasPrefix(a, "-"):
			die("unknown flag %s", a)
		case target == "":
			target = a
		default:
			die("publish takes one file or directory")
		}
	}
	if target == "" {
		die("usage: sendup publish <file|dir> [--name NAME] [--ttl DAYS] [--auth [user:pass]] [--no-auth] [--new]")
	}
	if wantAuth && noAuth {
		die("--auth and --no-auth contradict each other")
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		die("cannot resolve %s: %v", target, err)
	}

	if slug != "" {
		slug = strings.ToLower(slug)
		if !slugRe.MatchString(slug) {
			die("name must be lowercase letters, digits and hyphens, 2-63 chars — it becomes a hostname label")
		}
		if reserved[slug] {
			die("%q is reserved", slug)
		}
	}

	// One scan of the site records covers both jobs: finding the slug this source was
	// last published to, and recovering that site's settings.
	index := map[string]map[string]any{}
	if !forceNew {
		for _, k := range listKeys(cfg, "__index:") {
			index[strings.TrimPrefix(k.Name, "__index:")] = k.Metadata
		}
	}

	reused := false
	if slug == "" && !forceNew {
		// most recent wins if the same source was somehow published twice
		best := ""
		for s, m := range index {
			if metaString(m, "path") != absTarget {
				continue
			}
			if best == "" || metaString(m, "created") > metaString(index[best], "created") {
				best = s
			}
		}
		if best != "" {
			slug, reused = best, true
		}
	}
	if slug == "" {
		slug = newSlug()
	}
	prevMeta, existed := index[slug]
	if existed && !reused {
		reused = true // explicit --slug pointing at a site that already exists
	}

	var ttl int64
	if ttlDays > 0 {
		ttl = max(int64(ttlDays*86400), 60) // 60s is the KV minimum
	}

	// link secret: keep the existing one so links already sent keep working, unless
	// asked to rotate (which revokes every link issued so far)
	linkSecret := ""
	if !noLink {
		if prev, ok := prevMeta["link"].(string); ok && !rotate {
			linkSecret = prev
		} else if wantLink || rotate {
			linkSecret = newSecret()
		}
	}

	var authMeta map[string]any
	authCarried := false
	switch {
	case wantAuth:
		if authUser == "" {
			authUser, authPass = "sendup", newPassword()
		}
		salt, hash := hashCreds(authUser, authPass)
		authMeta = map[string]any{"salt": salt, "hash": hash}
	case noAuth:
		// explicit opt-out; leave authMeta nil so the republish drops the gate
	default:
		// republishing must never silently unprotect a site
		if prev, ok := prevMeta["auth"].(map[string]any); ok {
			authMeta, authCarried = prev, true
		}
	}

	files := collectFiles(target)
	pairs := make([]kvPair, 0, len(files)+1)
	var total int64
	title := ""

	for _, f := range files {
		if f.size > maxValue {
			die("%s is %s, over the 25 MB per-file KV limit", f.path, fmtBytes(f.size))
		}
		buf, err := os.ReadFile(f.abs)
		if err != nil {
			die("cannot read %s: %v", f.abs, err)
		}
		total += int64(len(buf))
		sum := sha256.Sum256(buf)
		if f.path == "/index.html" {
			title = pageTitle(buf)
		}
		// access config lives only on the site record, so the dashboard can change it
		// with one write instead of rewriting every file
		pairs = append(pairs, kvPair{
			Key:           slug + ":" + f.path,
			Value:         base64.StdEncoding.EncodeToString(buf),
			Base64:        true,
			Metadata:      map[string]any{"etag": `"` + hex.EncodeToString(sum[:])[:32] + `"`},
			ExpirationTTL: ttl,
		})
	}

	meta := map[string]any{
		"created": time.Now().UTC().Format(time.RFC3339),
		"files":   len(files),
		"bytes":   total,
		"src":     filepath.Base(absTarget),
		"path":    absTarget, // how a later publish finds its way back to this slug
	}
	if title != "" {
		meta["title"] = title
	}
	if ttl > 0 {
		meta["expires"] = time.Now().UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
	}
	// also on the site record, so the worker can gate 404s and paths can't be probed
	if authMeta != nil {
		meta["auth"] = authMeta
	}
	if linkSecret != "" {
		meta["link"] = linkSecret
	}
	pairs = append(pairs, kvPair{Key: "__index:" + slug, Value: "", Metadata: meta, ExpirationTTL: ttl})

	bulkPut(cfg, pairs)

	// drop files the previous publish left behind, otherwise deleted pages keep serving
	swept := 0
	if reused {
		fresh := make(map[string]bool, len(files))
		for _, f := range files {
			fresh[slug+":"+f.path] = true
		}
		var stale []string
		for _, k := range listKeys(cfg, slug+":") {
			if !fresh[k.Name] {
				stale = append(stale, k.Name)
			}
		}
		if len(stale) > 0 {
			body, _ := json.Marshal(stale)
			cf(cfg, http.MethodPost,
				fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/bulk/delete", cfg.AccountID, cfg.NamespaceID),
				body, false)
			swept = len(stale)
		}
	}

	rememberPublished(absTarget, slug) // so `sendup pull` can spot a later rename

	fmt.Printf("https://%s.%s/\n", slug, cfg.Domain)
	extra := ""
	if ttl > 0 {
		extra = fmt.Sprintf(", expires in %g days", ttlDays)
	}
	where := "new site"
	if reused {
		where = "updated in place"
		if swept > 0 {
			where += fmt.Sprintf(", %d stale file%s removed", swept, map[bool]string{true: "", false: "s"}[swept == 1])
		}
	}
	fmt.Fprintf(os.Stderr, "  %d file%s, %s%s (%s)\n", len(files), plural(len(files)), fmtBytes(total), extra, where)

	switch {
	case authMeta != nil && authCarried:
		fmt.Fprintf(os.Stderr, "  basic auth  kept from the previous publish (--no-auth to remove)\n")
	case authMeta != nil:
		// only the salted hash reaches KV — this is the one time the password is shown
		fmt.Fprintf(os.Stderr, "  basic auth  %s / %s\n", authUser, authPass)
		fmt.Fprintf(os.Stderr, "  not recoverable later — republish to change it\n")
	case noAuth && reused:
		fmt.Fprintf(os.Stderr, "  basic auth  removed — the page is now open to anyone with the URL\n")
	}

	switch {
	case linkSecret != "" && (wantLink || rotate):
		var exp int64
		if linkTTLDays > 0 {
			exp = time.Now().Add(time.Duration(linkTTLDays * float64(24*time.Hour))).Unix()
		}
		if rotate {
			fmt.Fprintf(os.Stderr, "  signed link rotated — every link issued before now is dead\n")
		}
		fmt.Fprintf(os.Stderr, "  share this:\n")
		fmt.Printf("%s\n", linkURL(cfg, slug, mintToken(linkSecret, slug, exp)))
		if exp > 0 {
			fmt.Fprintf(os.Stderr, "  link expires %s\n", time.Unix(exp, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
	case linkSecret != "":
		fmt.Fprintf(os.Stderr, "  signed link required — `sendup link %s` to issue one\n", slug)
	case noLink && reused:
		fmt.Fprintf(os.Stderr, "  signed link requirement removed\n")
	}
}

func cmdList(args []string) {
	if len(args) > 0 {
		die("list takes no arguments")
	}
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	keys := listKeys(cfg, "__index:")
	if len(keys) == 0 {
		fmt.Println("nothing published yet")
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		return metaString(keys[i].Metadata, "created") > metaString(keys[j].Metadata, "created")
	})

	for _, k := range keys {
		slug := strings.TrimPrefix(k.Name, "__index:")
		when := "?"
		if c := metaString(k.Metadata, "created"); len(c) >= 16 {
			when = strings.Replace(c[:16], "T", " ", 1)
		}
		count := "?"
		if n, ok := metaNumber(k.Metadata, "files"); ok {
			count = strconv.Itoa(int(n))
		}
		size := "?"
		if n, ok := metaNumber(k.Metadata, "bytes"); ok {
			size = fmtBytes(int64(n))
		}
		expires := ""
		if e := metaString(k.Metadata, "expires"); len(e) >= 10 {
			expires = "  (expires " + e[:10] + ")"
		}
		fmt.Printf("%s  %3s files  %8s  https://%s.%s/  %s%s\n",
			when, count, size, slug, cfg.Domain, metaString(k.Metadata, "src"), expires)
	}
}

// cmdToken swaps the stored API token without calling Cloudflare, so a narrower
// publish-only token can replace the one `init` needed.
func cmdToken(args []string) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		die("usage: sendup token <TOKEN>")
	}
	cfg := loadConfig()
	cfg.require("accountId", "namespaceId", "domain")
	cfg.APIToken = args[0]
	cfg.save()
	fmt.Fprintf(os.Stderr, "stored in %s\n", configPath())
}

func cmdAdmin(args []string) {
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	user, pass := "admin", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--password":
			i++
			if i < len(args) {
				pass = args[i]
			}
		case "--user":
			i++
			if i < len(args) {
				user = args[i]
			}
		default:
			die("unknown argument %s", args[i])
		}
	}
	if pass == "" {
		pass = newPassword()
	}

	salt, hash := hashCreds(user, pass)
	body, _ := json.Marshal([]kvPair{{
		Key:      "__admin",
		Value:    "",
		Metadata: map[string]any{"salt": salt, "hash": hash},
	}})
	cf(cfg, http.MethodPut,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/bulk", cfg.AccountID, cfg.NamespaceID),
		body, false)

	fmt.Printf("https://dash.%s/\n", cfg.Domain)
	fmt.Fprintf(os.Stderr, "  login  %s / %s\n", user, pass)
	fmt.Fprintf(os.Stderr, "  only the salted hash is stored — rerun this to change it\n")
}

func cmdLink(args []string) {
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	slug := ""
	ttlDays := 0.0
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					die("--ttl wants a positive number of days")
				}
				ttlDays = v
			}
		case strings.HasPrefix(a, "-"):
			die("unknown flag %s", a)
		case slug == "":
			slug = a
		default:
			die("link takes one slug")
		}
	}
	if slug == "" {
		die("usage: sendup link <slug> [--ttl DAYS]")
	}

	var meta map[string]any
	for _, k := range listKeys(cfg, "__index:"+slug) {
		if k.Name == "__index:"+slug {
			meta = k.Metadata
		}
	}
	if meta == nil {
		die("no site with slug %s", slug)
	}
	secret, ok := meta["link"].(string)
	if !ok || secret == "" {
		die("%s does not use signed links — republish it with --link first", slug)
	}

	var exp int64
	if ttlDays > 0 {
		exp = time.Now().Add(time.Duration(ttlDays * float64(24*time.Hour))).Unix()
	}
	fmt.Println(linkURL(cfg, slug, mintToken(secret, slug, exp)))
	if exp > 0 {
		fmt.Fprintf(os.Stderr, "  expires %s\n", time.Unix(exp, 0).UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		fmt.Fprintf(os.Stderr, "  no expiry — `sendup publish ... --rotate` revokes every issued link\n")
	}
}

// cmdRename moves every key of a site to a new prefix. KV has no rename, so this is a
// read + write + delete per file.
func cmdRename(args []string) {
	if len(args) != 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		die("usage: sendup rename <slug> <new-name>")
	}
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	from := args[0]
	to := strings.ToLower(strings.TrimSpace(args[1]))
	if !slugRe.MatchString(to) {
		die("name must be lowercase letters, digits and hyphens, 2-63 chars")
	}
	if reserved[to] {
		die("%q is reserved", to)
	}
	if to == from {
		die("that is already its name")
	}

	var meta map[string]any
	for _, k := range listKeys(cfg, "__index:"+from) {
		if k.Name == "__index:"+from {
			meta = k.Metadata
		}
	}
	if meta == nil {
		die("no site with slug %s", from)
	}
	for _, k := range listKeys(cfg, "__index:"+to) {
		if k.Name == "__index:"+to {
			die("%s is already taken", to)
		}
	}

	base := fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s", cfg.AccountID, cfg.NamespaceID)
	files := listKeys(cfg, from+":")

	pairs := make([]kvPair, 0, len(files)+1)
	old := make([]string, 0, len(files)+1)
	for _, k := range files {
		value := kvValue(cfg, base, k.Name)
		pairs = append(pairs, kvPair{
			Key:      to + ":" + strings.TrimPrefix(k.Name, from+":"),
			Value:    base64.StdEncoding.EncodeToString(value),
			Base64:   true,
			Metadata: k.Metadata,
		})
		old = append(old, k.Name)
	}
	pairs = append(pairs, kvPair{Key: "__index:" + to, Value: "", Metadata: meta})
	old = append(old, "__index:"+from)

	bulkPut(cfg, pairs)
	body, _ := json.Marshal(old)
	cf(cfg, http.MethodPost, base+"/bulk/delete", body, false)

	if p := metaString(meta, "path"); p != "" {
		rememberPublished(p, to)
	}
	fmt.Printf("https://%s.%s/\n", to, cfg.Domain)
	fmt.Fprintf(os.Stderr, "  moved %d file%s from %s\n", len(files), plural(len(files)), from)
	if _, gated := meta["link"]; gated {
		// tokens are HMACed over the slug, so the old ones can never validate again
		fmt.Fprintf(os.Stderr, "  every signed link issued for %s is now dead\n", from)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// kvValue fetches one key's raw bytes.
func kvValue(cfg *Config, base, key string) []byte {
	req, err := http.NewRequest(http.MethodGet, apiBase+base+"/values/"+url.PathEscape(key), nil)
	if err != nil {
		die("bad request for %s: %v", key, err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	res, err := httpClient.Do(req)
	if err != nil {
		die("fetching %s failed: %v", key, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		die("reading %s failed: %v", key, err)
	}
	if res.StatusCode != http.StatusOK {
		die("API %d reading %s: %s", res.StatusCode, key, truncate(string(raw), 200))
	}
	return raw
}

// cmdPull reports what changed on the server since this machine last published —
// renames or deletions done from the dashboard or another machine.
func cmdPull(args []string) {
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")

	only := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			die("unknown flag %s", a)
		}
		if only != "" {
			die("usage: sendup pull [file|dir]")
		}
		abs, err := filepath.Abs(a)
		if err != nil {
			die("cannot resolve %s: %v", a, err)
		}
		only = abs
	}

	// server truth: source path -> slug
	live := map[string]string{}
	slugs := map[string]bool{}
	for _, k := range listKeys(cfg, "__index:") {
		slug := strings.TrimPrefix(k.Name, "__index:")
		slugs[slug] = true
		if p := metaString(k.Metadata, "path"); p != "" {
			live[p] = slug
		}
	}

	known := loadPublished()
	paths := make([]string, 0, len(known)+len(live))
	seen := map[string]bool{}
	for p := range known {
		if !seen[p] {
			paths, seen[p] = append(paths, p), true
		}
	}
	for p := range live {
		if !seen[p] {
			paths, seen[p] = append(paths, p), true
		}
	}
	sort.Strings(paths)

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
			fmt.Printf("renamed    %s\n           %s -> https://%s.%s/\n", p, was, now, cfg.Domain)
			known[p] = now
		case hadLocal && !onServer:
			changes++
			fmt.Printf("gone       %s\n           %s no longer exists\n", p, was)
			delete(known, p)
		case !hadLocal && onServer:
			changes++
			fmt.Printf("new here   %s\n           https://%s.%s/\n", p, now, cfg.Domain)
			known[p] = now
		default:
			fmt.Printf("unchanged  %s\n           https://%s.%s/\n", p, now, cfg.Domain)
		}
	}

	if only != "" && !seen[only] {
		die("%s has never been published from this machine, and no site records it as its source", only)
	}
	known.save()

	if changes == 0 {
		fmt.Fprintln(os.Stderr, "\nnothing changed")
	} else {
		fmt.Fprintf(os.Stderr, "\n%d change%s, local record updated\n", changes, plural(changes))
	}
}

// cmdDoctor compares local config against what Cloudflare actually has, and against the
// generated wrangler.jsonc. Drift between the three is silent otherwise — a stale
// namespace id points every command at an empty store while the real data sits elsewhere.
func cmdDoctor(args []string) {
	fix := false
	for _, a := range args {
		if a == "--fix" {
			fix = true
		} else {
			die("unknown argument %s", a)
		}
	}

	cfg := loadConfig()
	problems := 0
	report := func(state, label, detail string) {
		if state == "FAIL" {
			problems++
		}
		fmt.Printf("%-5s %-22s %s\n", state, label, detail)
	}

	if cfg.APIToken == "" || cfg.AccountID == "" || cfg.NamespaceID == "" || cfg.Domain == "" {
		die("config is incomplete — run `sendup init --domain <your-domain> --token <TOKEN>`")
	}
	report("ok", "config", configPath())
	report("ok", "domain", cfg.Domain)

	// zone still ours, and still the id we recorded?
	if resp := cf(cfg, http.MethodGet, "/zones?name="+url.QueryEscape(cfg.Domain), nil, true); resp == nil {
		report("warn", "zone", "token cannot read zones — skipped (needs Zone > Zone > Read)")
	} else {
		var zones []struct {
			ID      string `json:"id"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}
		if err := json.Unmarshal(resp.Result, &zones); err != nil {
			die("unexpected zone listing: %v", err)
		}
		switch {
		case len(zones) == 0:
			report("FAIL", "zone", fmt.Sprintf("%s is not a zone on this account any more", cfg.Domain))
		case zones[0].ID != cfg.ZoneID:
			report("FAIL", "zone", fmt.Sprintf("id changed: config %s, Cloudflare %s", cfg.ZoneID, zones[0].ID))
		case zones[0].Account.ID != cfg.AccountID:
			report("FAIL", "account", fmt.Sprintf("id changed: config %s, Cloudflare %s", cfg.AccountID, zones[0].Account.ID))
		default:
			report("ok", "zone", cfg.ZoneID)
		}
	}

	// namespace still exists under the recorded id?
	resp := cf(cfg, http.MethodGet,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces?per_page=100", cfg.AccountID), nil, true)
	if resp == nil {
		report("warn", "kv namespace", "token cannot list namespaces — skipped")
	} else {
		var namespaces []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(resp.Result, &namespaces); err != nil {
			die("unexpected namespace listing: %v", err)
		}
		title := ""
		for _, n := range namespaces {
			if n.ID == cfg.NamespaceID {
				title = n.Title
			}
		}
		if title == "" {
			others := make([]string, 0, len(namespaces))
			for _, n := range namespaces {
				others = append(others, n.Title+" "+n.ID)
			}
			report("FAIL", "kv namespace", fmt.Sprintf("%s does not exist. On the account: %s",
				cfg.NamespaceID, strings.Join(others, ", ")))
		} else {
			report("ok", "kv namespace", title+" "+cfg.NamespaceID)
		}
	}

	// wildcard DNS
	name := "*." + cfg.Domain
	if resp := cf(cfg, http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?name=%s", cfg.ZoneID, url.QueryEscape(name)), nil, true); resp == nil {
		report("warn", "wildcard dns", "token cannot read DNS — skipped")
	} else {
		var records []struct {
			Type    string `json:"type"`
			Proxied bool   `json:"proxied"`
		}
		if err := json.Unmarshal(resp.Result, &records); err != nil {
			die("unexpected dns listing: %v", err)
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

	// wrangler.jsonc agreeing with the config it was generated from
	if raw, err := os.ReadFile("wrangler.jsonc"); err != nil {
		report("warn", "wrangler.jsonc", "not in this directory — run doctor from the repo root")
	} else {
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
		if len(drift) == 0 {
			report("ok", "wrangler.jsonc", "matches config")
		} else if fix {
			patchWrangler(cfg)
			report("ok", "wrangler.jsonc", "rewrote "+strings.Join(drift, ", "))
		} else {
			report("FAIL", "wrangler.jsonc", strings.Join(drift, ", ")+" disagree with config — `sendup doctor --fix`")
		}
	}

	// dashboard set up?
	admin := false
	for _, k := range listKeys(cfg, "__admin") {
		if k.Name == "__admin" {
			admin = true
		}
	}
	if admin {
		report("ok", "dashboard", "https://dash."+cfg.Domain+"/")
	} else {
		report("warn", "dashboard", "not set up — `sendup admin` if you want it")
	}

	// is a worker actually answering on the wildcard?
	probe := fmt.Sprintf("https://%s.%s/", newSlug(), cfg.Domain)
	if res, err := httpClient.Get(probe); err != nil {
		report("FAIL", "worker", "no response from "+probe+": "+err.Error())
	} else {
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode == http.StatusNotFound && strings.Contains(string(body), "no such site") {
			report("ok", "worker", "serving on *."+cfg.Domain)
		} else {
			report("FAIL", "worker",
				fmt.Sprintf("%s answered %d, not the worker's 404 — deploy may be missing or the route unbound",
					probe, res.StatusCode))
		}
	}

	if problems > 0 {
		fmt.Fprintf(os.Stderr, "\n%d problem%s\n", problems, plural(problems))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "\nall good")
}

func cmdRm(args []string) {
	if len(args) != 1 {
		die("usage: sendup rm <slug>")
	}
	cfg := loadConfig()
	cfg.require("apiToken", "accountId", "namespaceId", "domain")
	slug := args[0]

	var names []string
	for _, k := range listKeys(cfg, slug+":") {
		names = append(names, k.Name)
	}
	for _, k := range listKeys(cfg, "__index:"+slug) {
		if k.Name == "__index:"+slug {
			names = append(names, k.Name)
		}
	}
	if len(names) == 0 {
		die("no site with slug %s", slug)
	}

	body, _ := json.Marshal(names)
	cf(cfg, http.MethodPost,
		fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/bulk/delete", cfg.AccountID, cfg.NamespaceID),
		body, false)
	fmt.Printf("removed %s (%d files)\n", slug, len(names)-1)
}

func usage() {
	fmt.Printf(`sendup — publish static pages to *.%s

  sendup publish <file|dir> [--name NAME] [--ttl DAYS]
                            [--auth [user:pass]] [--no-auth]
                            [--link] [--link-ttl DAYS] [--no-link] [--rotate]
                            [--new]
  sendup link <slug> [--ttl DAYS]
  sendup list
  sendup rm <slug>
  sendup rename <slug> <new-name>
  sendup pull [file|dir]                         what changed on the server since you published
  sendup doctor [--fix]                          check local config against Cloudflare
  sendup admin [--user NAME] [--password PASS]   set up the dashboard
  sendup token <TOKEN>                           swap the stored API token
  sendup init --domain <your-domain> [--token TOKEN] [--skip-dns]

A single .html file becomes the site root. A directory is uploaded as-is;
index.html at its root becomes "/".

The subdomain defaults to 16 random hex chars. --name picks it by hand, e.g.
--name docs serves https://docs.<your-domain>/ (--slug is accepted as an alias).

Publishing the same file or directory again reuses the subdomain it went to
last time, so the URL you shared keeps working. Files removed since the last
publish are swept. --new forces a fresh subdomain.

Three ways to control who gets in, and they compose:

  (default)  the random subdomain IS the secret — anyone with the URL, no prompt
  --link     only a signed link opens the page; no password to type. --link-ttl
             sets an expiry, --rotate kills every link issued so far, and
             "sendup link <slug>" mints more without re-uploading
  --auth     HTTP Basic auth; bare --auth generates the credentials, prints once

With both --link and --auth, either one gets you in. A republish keeps whatever
the site already had; --no-auth and --no-link remove them.
`, domainOr("<your-domain>"))
}

// domainOr reports the configured domain, or a placeholder before `init` has run.
func domainOr(fallback string) string {
	if d := loadConfig().Domain; d != "" {
		return d
	}
	return fallback
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "publish", "p":
		cmdPublish(args)
	case "list", "ls":
		cmdList(args)
	case "link":
		cmdLink(args)
	case "admin":
		cmdAdmin(args)
	case "token":
		cmdToken(args)
	case "rm", "delete":
		cmdRm(args)
	case "rename", "mv":
		cmdRename(args)
	case "doctor", "check":
		cmdDoctor(args)
	case "pull", "sync":
		cmdPull(args)
	case "init":
		cmdInit(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}
