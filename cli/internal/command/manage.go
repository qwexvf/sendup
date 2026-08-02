package command

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"sendup/internal/cf"
	"sendup/internal/config"
	"sendup/internal/site"
)

func List(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("list takes no arguments")
	}
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

	keys, err := api.ListKeys("__index:")
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("nothing published yet")
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		return cf.MetaString(keys[i].Metadata, "created") > cf.MetaString(keys[j].Metadata, "created")
	})

	for _, k := range keys {
		slug := strings.TrimPrefix(k.Name, "__index:")
		m := k.Metadata

		when := "?"
		if c := cf.MetaString(m, "created"); len(c) >= 16 {
			when = strings.Replace(c[:16], "T", " ", 1)
		}
		count := "?"
		if n, ok := cf.MetaNumber(m, "files"); ok {
			count = strconv.Itoa(int(n))
		}
		size := "?"
		if n, ok := cf.MetaNumber(m, "bytes"); ok {
			size = site.FmtBytes(int64(n))
		}
		expires := ""
		if e := cf.MetaString(m, "expires"); len(e) >= 10 {
			expires = "  (expires " + e[:10] + ")"
		}
		fmt.Printf("%s  %3s files  %8s  %s  %s%s\n",
			when, count, size, siteURL(cfg, slug), cf.MetaString(m, "src"), expires)
	}
	return nil
}

func Remove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: sendup rm <slug>")
	}
	_, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}
	slug := args[0]

	files, err := api.ListKeys(slug + ":")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files)+1)
	for _, k := range files {
		names = append(names, k.Name)
	}
	index, err := api.ListKeys("__index:" + slug)
	if err != nil {
		return err
	}
	for _, k := range index {
		if k.Name == "__index:"+slug {
			names = append(names, k.Name)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no site with slug %s", slug)
	}

	if err := api.BulkDelete(names); err != nil {
		return err
	}
	fmt.Printf("removed %s (%d files)\n", slug, len(names)-1)
	return nil
}

// Rename moves every key of a site to a new prefix. KV has no rename, so this is a read
// + write + delete per file.
func Rename(args []string) error {
	if len(args) != 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("usage: sendup rename <slug> <new-name>")
	}
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

	from := args[0]
	to, err := site.ValidateSlug(args[1])
	if err != nil {
		return err
	}
	if to == from {
		return fmt.Errorf("that is already its name")
	}

	meta, err := record(api, from)
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("no site with slug %s", from)
	}
	taken, err := record(api, to)
	if err != nil {
		return err
	}
	if taken != nil {
		return fmt.Errorf("%s is already taken", to)
	}

	files, err := api.ListKeys(from + ":")
	if err != nil {
		return err
	}
	pairs := make([]cf.Pair, 0, len(files)+1)
	old := make([]string, 0, len(files)+1)
	for _, k := range files {
		value, err := api.Value(k.Name)
		if err != nil {
			return err
		}
		pairs = append(pairs, cf.Pair{
			Key:      to + ":" + strings.TrimPrefix(k.Name, from+":"),
			Value:    encodeBase64(value),
			Base64:   true,
			Metadata: k.Metadata,
		})
		old = append(old, k.Name)
	}
	pairs = append(pairs, cf.Pair{Key: "__index:" + to, Value: "", Metadata: meta})
	old = append(old, "__index:"+from)

	if err := api.BulkPut(pairs); err != nil {
		return err
	}
	if err := api.BulkDelete(old); err != nil {
		return err
	}

	if p := cf.MetaString(meta, "path"); p != "" {
		config.Remember(p, to)
	}
	fmt.Println(siteURL(cfg, to))
	note("  moved %d file%s from %s", len(files), site.Plural(len(files)), from)
	if _, gated := meta["link"]; gated {
		// tokens are HMACed over the slug, so the old ones can never validate again
		note("  every signed link issued for %s is now dead", from)
	}
	return nil
}

func Link(args []string) error {
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

	slug := ""
	ttlDays := 0.0
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					return fmt.Errorf("--ttl wants a positive number of days")
				}
				ttlDays = v
			}
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %s", a)
		case slug == "":
			slug = a
		default:
			return fmt.Errorf("link takes one slug")
		}
	}
	if slug == "" {
		return fmt.Errorf("usage: sendup link <slug> [--ttl DAYS]")
	}

	meta, err := record(api, slug)
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("no site with slug %s", slug)
	}
	secret, ok := meta["link"].(string)
	if !ok || secret == "" {
		return fmt.Errorf("%s does not use signed links — republish it with --link first", slug)
	}

	var exp int64
	if ttlDays > 0 {
		exp = time.Now().Add(time.Duration(ttlDays * float64(24*time.Hour))).Unix()
	}
	token, err := site.MintToken(secret, slug, exp)
	if err != nil {
		return err
	}
	fmt.Println(linkURL(cfg, slug, token))
	if exp > 0 {
		note("  expires %s", time.Unix(exp, 0).UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		note("  no expiry — `sendup publish ... --rotate` revokes every issued link")
	}
	return nil
}

// Admin sets the dashboard credentials.
func Admin(args []string) error {
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

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
			return fmt.Errorf("unknown argument %s", args[i])
		}
	}
	if pass == "" {
		if pass, err = site.NewPassword(); err != nil {
			return err
		}
	}

	salt, hash, err := site.HashCreds(user, pass)
	if err != nil {
		return err
	}
	if err := api.BulkPut([]cf.Pair{{
		Key:      "__admin",
		Value:    "",
		Metadata: map[string]any{"salt": salt, "hash": hash},
	}}); err != nil {
		return err
	}

	fmt.Printf("https://dash.%s/\n", cfg.Domain)
	note("  login  %s / %s", user, pass)
	note("  only the salted hash is stored — rerun this to change it")
	return nil
}

// Token swaps the stored API token without calling Cloudflare, so a narrower
// publish-only token can replace the one `init` needed.
func Token(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: sendup token <TOKEN>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Require("accountId", "namespaceId", "domain"); err != nil {
		return err
	}
	cfg.APIToken = args[0]
	if err := cfg.Save(); err != nil {
		return err
	}
	note("stored in %s", config.Path())
	return nil
}

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
