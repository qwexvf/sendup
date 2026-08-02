// devdox — publish static pages to <random>.example.com
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	fmt.Fprintf(os.Stderr, "devdox: "+format+"\n", a...)
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
		return filepath.Join(d, "devdox")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		die("cannot locate home directory: %v", err)
	}
	return filepath.Join(home, ".config", "devdox")
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
	if d := os.Getenv("DEVDOX_DOMAIN"); d != "" {
		cfg.Domain = d
	}
	return cfg
}

func (c *Config) save() {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		die("cannot create %s: %v", configDir(), err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		die("cannot encode config: %v", err)
	}
	if err := os.WriteFile(configPath(), append(data, '\n'), 0o600); err != nil {
		die("cannot write %s: %v", configPath(), err)
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
			die("missing %q — run `devdox init` first (or set it in %s)", f, configPath())
		}
	}
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

func newSlug() string {
	var b [8]byte // 64 bits
	if _, err := rand.Read(b[:]); err != nil {
		die("cannot generate slug: %v", err)
	}
	return hex.EncodeToString(b[:])
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
		fmt.Fprintln(os.Stderr, `devdox: warning — no index.html at the root, "/" will 404`)
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
	domain := "example.com"
	if cfg.Domain != "" {
		domain = cfg.Domain
	}
	nsTitle := "devdox-sites"
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
	cfg.Domain = domain

	if cfg.APIToken == "" {
		die("no API token. Create one at https://dash.cloudflare.com/profile/api-tokens with:\n" +
			"  Account > Workers KV Storage > Edit\n" +
			"  Account > Workers Scripts    > Edit\n" +
			"  Zone    > Workers Routes     > Edit\n" +
			"  Zone    > Zone               > Read\n" +
			"  Zone    > DNS                > Edit   (optional, see --skip-dns)\n" +
			"then: devdox init --token <TOKEN>   (or export CLOUDFLARE_API_TOKEN=...)")
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
	for _, n := range namespaces {
		if n.Title == nsTitle {
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
	fmt.Printf("\nnext:\n  wrangler deploy   (needs Workers Scripts + Workers Routes on the token)\n")
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
		"comment": "devdox — wildcard for worker route",
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
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--slug":
			i++
			if i < len(args) {
				slug = args[i]
			}
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
		die("usage: devdox publish <file|dir> [--slug NAME] [--ttl DAYS]")
	}

	if slug != "" {
		slug = strings.ToLower(slug)
		if !slugRe.MatchString(slug) {
			die("--slug must be lowercase letters, digits and hyphens")
		}
	} else {
		slug = newSlug()
	}

	var ttl int64
	if ttlDays > 0 {
		ttl = max(int64(ttlDays*86400), 60) // 60s is the KV minimum
	}

	files := collectFiles(target)
	pairs := make([]kvPair, 0, len(files)+1)
	var total int64

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
		pairs = append(pairs, kvPair{
			Key:           slug + ":" + f.path,
			Value:         base64.StdEncoding.EncodeToString(buf),
			Base64:        true,
			Metadata:      map[string]any{"etag": `"` + hex.EncodeToString(sum[:])[:32] + `"`},
			ExpirationTTL: ttl,
		})
	}

	absTarget, _ := filepath.Abs(target)
	meta := map[string]any{
		"created": time.Now().UTC().Format(time.RFC3339),
		"files":   len(files),
		"bytes":   total,
		"src":     filepath.Base(absTarget),
	}
	if ttl > 0 {
		meta["expires"] = time.Now().UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
	}
	pairs = append(pairs, kvPair{Key: "__index:" + slug, Value: "", Metadata: meta, ExpirationTTL: ttl})

	bulkPut(cfg, pairs)

	fmt.Printf("https://%s.%s/\n", slug, cfg.Domain)
	plural := "s"
	if len(files) == 1 {
		plural = ""
	}
	extra := ""
	if ttl > 0 {
		extra = fmt.Sprintf(", expires in %g days", ttlDays)
	}
	fmt.Fprintf(os.Stderr, "  %d file%s, %s%s\n", len(files), plural, fmtBytes(total), extra)
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

func cmdRm(args []string) {
	if len(args) != 1 {
		die("usage: devdox rm <slug>")
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
	fmt.Println(`devdox — publish static pages to *.example.com

  devdox publish <file|dir> [--slug NAME] [--ttl DAYS]
  devdox list
  devdox rm <slug>
  devdox init [--token TOKEN] [--domain example.com] [--skip-dns]

A single .html file becomes the site root. A directory is uploaded as-is;
index.html at its root becomes "/". Slugs are 16 random hex chars unless
--slug is given. Pages are public to anyone with the URL and sent with
X-Robots-Tag: noindex.`)
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
	case "rm", "delete":
		cmdRm(args)
	case "init":
		cmdInit(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}
