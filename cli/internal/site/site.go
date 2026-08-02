// Package site holds the rules that define a published site: what a valid subdomain
// looks like, how share tokens are signed, how credentials are hashed, and which files
// on disk make up an upload. Nothing here touches the network.
package site

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A slug becomes a DNS label, so it lives by hostname rules.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)

// dash.<domain> is the dashboard; www/api are the names people reach for by reflex.
var reserved = map[string]bool{"dash": true, "www": true, "api": true}

// ValidateSlug normalises and checks a user-supplied name.
func ValidateSlug(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !slugRe.MatchString(s) {
		return "", fmt.Errorf("name must be lowercase letters, digits and hyphens, 2-63 chars — it becomes a hostname label")
	}
	if reserved[s] {
		return "", fmt.Errorf("%q is reserved", s)
	}
	return s, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("no randomness available: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NewSlug is 64 bits of randomness — unguessable, and the default privacy model.
func NewSlug() (string, error) { return randomHex(8) }

// NewSecret keys the HMAC behind a site's share links.
func NewSecret() (string, error) { return randomHex(32) }

func NewPassword() (string, error) {
	b := make([]byte, 12) // 96 bits
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("no randomness available: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MintToken builds "<exp>.<sig>" where exp is unix seconds (0 = never) and sig is the
// first 16 bytes of HMAC-SHA256(secret, "<slug>:<exp>"). The expiry sits inside the
// signed message, so a recipient cannot extend their own link.
func MintToken(secretHex, slug string, exp int64) (string, error) {
	key, err := hex.DecodeString(secretHex)
	if err != nil {
		return "", fmt.Errorf("stored link secret is corrupt")
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s:%d", slug, exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(mac.Sum(nil)[:16])), nil
}

// HashCreds returns a random salt and sha256(salt + "user:pass"), both hex. The worker
// recomputes this from the Basic auth header and compares in constant time.
func HashCreds(user, pass string) (salt, hash string, err error) {
	salt, err = randomHex(16)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(salt + user + ":" + pass))
	return salt, hex.EncodeToString(sum[:]), nil
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// PageTitle pulls <title> out of an HTML document for the dashboard listing.
func PageTitle(buf []byte) string {
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

// Upload is one file destined for KV, at a site-absolute path.
type Upload struct {
	Path string // e.g. /index.html
	Abs  string
	Size int64
}

// Collect resolves a file or directory into the objects that make up a site. The second
// return is a warning worth showing the user, not an error.
func Collect(target string) ([]Upload, string, error) {
	st, err := os.Stat(target)
	if err != nil {
		return nil, "", fmt.Errorf("no such path: %s", target)
	}

	if !st.IsDir() {
		abs, _ := filepath.Abs(target)
		// a lone page always becomes the site root
		p := "/" + filepath.Base(target)
		switch strings.ToLower(filepath.Ext(target)) {
		case ".html", ".htm", ".md":
			p = "/index.html"
		}
		return []Upload{{Path: p, Abs: abs, Size: st.Size()}}, "", nil
	}

	root, _ := filepath.Abs(target)
	var out []Upload
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
		out = append(out, Upload{Path: "/" + filepath.ToSlash(rel), Abs: p, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("walking %s: %w", target, err)
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("no files under %s", target)
	}

	for _, f := range out {
		if f.Path == "/index.html" {
			return out, "", nil
		}
	}
	return out, `no index.html at the root, "/" will 404`, nil
}

func FmtBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func Plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
