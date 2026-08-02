package command

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sendup/internal/cf"
	"sendup/internal/config"
	"sendup/internal/site"
)

type publishOpts struct {
	target   string
	slug     string
	ttlDays  float64
	linkTTL  float64
	wantAuth bool
	noAuth   bool
	wantLink bool
	noLink   bool
	rotate   bool
	forceNew bool
	user     string
	pass     string
}

func parsePublish(args []string) (*publishOpts, error) {
	o := &publishOpts{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--slug", a == "--name":
			i++
			if i < len(args) {
				o.slug = args[i]
			}
		case a == "--auth":
			o.wantAuth = true
			// optional "user:pass"; bare --auth generates both
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				u, p, ok := strings.Cut(args[i], ":")
				if !ok || u == "" || p == "" {
					return nil, fmt.Errorf("--auth wants user:pass (or bare --auth to generate credentials)")
				}
				o.user, o.pass = u, p
			}
		case a == "--no-auth":
			o.noAuth = true
		case a == "--link":
			o.wantLink = true
		case a == "--no-link":
			o.noLink = true
		case a == "--rotate":
			o.rotate = true
		case a == "--link-ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					return nil, fmt.Errorf("--link-ttl wants a positive number of days")
				}
				o.linkTTL, o.wantLink = v, true
			}
		case a == "--new":
			o.forceNew = true
		case a == "--ttl":
			i++
			if i < len(args) {
				v, err := strconv.ParseFloat(args[i], 64)
				if err != nil || v <= 0 {
					return nil, fmt.Errorf("--ttl wants a positive number of days")
				}
				o.ttlDays = v
			}
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %s", a)
		case o.target == "":
			o.target = a
		default:
			return nil, fmt.Errorf("publish takes one file or directory")
		}
	}
	if o.target == "" {
		return nil, fmt.Errorf(
			"usage: sendup publish <file|dir> [--name NAME] [--ttl DAYS] [--auth [user:pass]] [--no-auth] [--new]")
	}
	if o.wantAuth && o.noAuth {
		return nil, fmt.Errorf("--auth and --no-auth contradict each other")
	}
	return o, nil
}

func Publish(args []string) error {
	o, err := parsePublish(args)
	if err != nil {
		return err
	}
	cfg, api, err := session("apiToken", "accountId", "namespaceId", "domain")
	if err != nil {
		return err
	}

	absTarget, err := filepath.Abs(o.target)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", o.target, err)
	}

	slug := ""
	if o.slug != "" {
		if slug, err = site.ValidateSlug(o.slug); err != nil {
			return err
		}
	}

	// One scan of the site records covers both jobs: finding the slug this source was
	// last published to, and recovering that site's settings.
	index := map[string]map[string]any{}
	if !o.forceNew {
		keys, err := api.ListKeys("__index:")
		if err != nil {
			return err
		}
		for _, k := range keys {
			index[strings.TrimPrefix(k.Name, "__index:")] = k.Metadata
		}
	}

	reused := false
	if slug == "" && !o.forceNew {
		// most recent wins if the same source was somehow published twice
		best := ""
		for s, m := range index {
			if cf.MetaString(m, "path") != absTarget {
				continue
			}
			if best == "" || cf.MetaString(m, "created") > cf.MetaString(index[best], "created") {
				best = s
			}
		}
		if best != "" {
			slug, reused = best, true
		}
	}
	if slug == "" {
		if slug, err = site.NewSlug(); err != nil {
			return err
		}
	}
	prevMeta, existed := index[slug]
	if existed && !reused {
		reused = true // explicit --name pointing at a site that already exists
	}

	var ttl int64
	if o.ttlDays > 0 {
		ttl = max(int64(o.ttlDays*86400), 60) // 60s is the KV minimum
	}

	// link secret: keep the existing one so links already sent keep working, unless
	// asked to rotate (which revokes every link issued so far)
	linkSecret := ""
	if !o.noLink {
		if prev, ok := prevMeta["link"].(string); ok && !o.rotate {
			linkSecret = prev
		} else if o.wantLink || o.rotate {
			if linkSecret, err = site.NewSecret(); err != nil {
				return err
			}
		}
	}

	var authMeta map[string]any
	authCarried := false
	switch {
	case o.wantAuth:
		if o.user == "" {
			o.user = "sendup"
			if o.pass, err = site.NewPassword(); err != nil {
				return err
			}
		}
		salt, hash, err := site.HashCreds(o.user, o.pass)
		if err != nil {
			return err
		}
		authMeta = map[string]any{"salt": salt, "hash": hash}
	case o.noAuth:
		// explicit opt-out; leave authMeta nil so the republish drops the gate
	default:
		// republishing must never silently unprotect a site
		if prev, ok := prevMeta["auth"].(map[string]any); ok {
			authMeta, authCarried = prev, true
		}
	}

	files, warning, err := site.Collect(o.target)
	if err != nil {
		return err
	}
	if warning != "" {
		note("sendup: warning — %s", warning)
	}

	pairs := make([]cf.Pair, 0, len(files)+1)
	var total int64
	title := ""

	for _, f := range files {
		if f.Size > cf.MaxValue {
			return fmt.Errorf("%s is %s, over the 25 MB per-file KV limit", f.Path, site.FmtBytes(f.Size))
		}
		buf, err := os.ReadFile(f.Abs)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", f.Abs, err)
		}
		total += int64(len(buf))
		if f.Path == "/index.html" {
			title = site.PageTitle(buf)
		}
		sum := sha256.Sum256(buf)
		// access config lives only on the site record, so the dashboard can change it
		// with one write instead of rewriting every file
		pairs = append(pairs, cf.Pair{
			Key:           slug + ":" + f.Path,
			Value:         encodeBase64(buf),
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
	pairs = append(pairs, cf.Pair{Key: "__index:" + slug, Value: "", Metadata: meta, ExpirationTTL: ttl})

	if err := api.BulkPut(pairs); err != nil {
		return err
	}

	// drop files the previous publish left behind, otherwise deleted pages keep serving
	swept := 0
	if reused {
		fresh := make(map[string]bool, len(files))
		for _, f := range files {
			fresh[slug+":"+f.Path] = true
		}
		existing, err := api.ListKeys(slug + ":")
		if err != nil {
			return err
		}
		var stale []string
		for _, k := range existing {
			if !fresh[k.Name] {
				stale = append(stale, k.Name)
			}
		}
		if len(stale) > 0 {
			if err := api.BulkDelete(stale); err != nil {
				return err
			}
			swept = len(stale)
		}
	}

	config.Remember(absTarget, slug) // so `sendup pull` can spot a later rename
	fmt.Println(siteURL(cfg, slug))
	reportPublish(cfg, o, slug, files, total, ttl, reused, swept, authMeta, authCarried, linkSecret)
	return nil
}

func reportPublish(
	cfg *config.Config, o *publishOpts, slug string, files []site.Upload,
	total, ttl int64, reused bool, swept int,
	authMeta map[string]any, authCarried bool, linkSecret string,
) {
	extra := ""
	if ttl > 0 {
		extra = fmt.Sprintf(", expires in %g days", o.ttlDays)
	}
	where := "new site"
	if reused {
		where = "updated in place"
		if swept > 0 {
			where += fmt.Sprintf(", %d stale file%s removed", swept, site.Plural(swept))
		}
	}
	note("  %d file%s, %s%s (%s)", len(files), site.Plural(len(files)), site.FmtBytes(total), extra, where)

	switch {
	case authMeta != nil && authCarried:
		note("  basic auth  kept from the previous publish (--no-auth to remove)")
	case authMeta != nil:
		// only the salted hash reaches KV — this is the one time the password is shown
		note("  basic auth  %s / %s", o.user, o.pass)
		note("  not recoverable later — republish to change it")
	case o.noAuth && reused:
		note("  basic auth  removed — the page is now open to anyone with the URL")
	}

	switch {
	case linkSecret != "" && (o.wantLink || o.rotate):
		var exp int64
		if o.linkTTL > 0 {
			exp = time.Now().Add(time.Duration(o.linkTTL * float64(24*time.Hour))).Unix()
		}
		if o.rotate {
			note("  signed link rotated — every link issued before now is dead")
		}
		token, err := site.MintToken(linkSecret, slug, exp)
		if err != nil {
			note("  could not mint a link: %v", err)
			return
		}
		note("  share this:")
		fmt.Println(linkURL(cfg, slug, token))
		if exp > 0 {
			note("  link expires %s", time.Unix(exp, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
	case linkSecret != "":
		note("  signed link required — `sendup link %s` to issue one", slug)
	case o.noLink && reused:
		note("  signed link requirement removed")
	}
}
