// Package cf is a small Cloudflare API client covering the handful of endpoints sendup
// needs: zones, DNS records, and Workers KV.
package cf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sendup/internal/config"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// KV limits. A value caps at 25 MB; a bulk request at 100 MB / 10k keys.
const (
	MaxValue     = 25 << 20
	maxBulkBytes = 90 << 20 // leave headroom under the 100 MB cap
	maxBulkKeys  = 10000
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

// HTTPClient is exposed so callers can probe a published site with the same timeouts.
func HTTPClient() *http.Client { return httpClient }

type Client struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Client { return &Client{cfg: cfg} }

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Resp struct {
	Success    bool            `json:"success"`
	Errors     []apiError      `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Cursor string `json:"cursor"`
	} `json:"result_info"`
}

// Forbidden marks a call the token has no permission for, so callers can degrade
// instead of failing — `init` skips DNS, `doctor` reports a warning.
type Forbidden struct{ Permission string }

func (e *Forbidden) Error() string {
	return fmt.Sprintf("the token is missing a permission for this call — it needs %s.\n"+
		"  edit it at https://dash.cloudflare.com/profile/api-tokens, then re-run the same command", e.Permission)
}

// permissionFor names the checkbox a 403 is asking for.
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

func (c *Client) Do(method, path string, body []byte) (*Resp, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, apiBase+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("bad request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s failed: %w", path, err)
	}

	var out Resp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("API %d on %s: %s", res.StatusCode, path, truncate(string(raw), 300))
	}
	if out.Success {
		return &out, nil
	}

	authFail := res.StatusCode == http.StatusForbidden
	msgs := make([]string, 0, len(out.Errors))
	for _, e := range out.Errors {
		if e.Code == 10000 {
			authFail = true
		}
		msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	if authFail {
		return nil, &Forbidden{Permission: permissionFor(path)}
	}

	detail := strings.Join(msgs, "; ")
	if detail == "" {
		detail = truncate(string(raw), 300)
	}
	return nil, fmt.Errorf("API %d on %s: %s", res.StatusCode, path, detail)
}

// Optional runs a call the token may not be allowed to make. A permission failure comes
// back as (nil, nil); anything else is a real error.
func (c *Client) Optional(method, path string, body []byte) (*Resp, error) {
	resp, err := c.Do(method, path, body)
	var forbidden *Forbidden
	if errors.As(err, &forbidden) {
		return nil, nil
	}
	return resp, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------- Workers KV ----------

func (c *Client) kvBase() string {
	return fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s", c.cfg.AccountID, c.cfg.NamespaceID)
}

type Pair struct {
	Key           string         `json:"key"`
	Value         string         `json:"value"`
	Base64        bool           `json:"base64,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	ExpirationTTL int64          `json:"expiration_ttl,omitempty"`
}

type Key struct {
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata"`
}

// BulkPut chunks so no single request blows the 100 MB / 10k-key ceiling.
func (c *Client) BulkPut(pairs []Pair) error {
	batch := make([]Pair, 0, len(pairs))
	size := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		body, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("cannot encode upload: %w", err)
		}
		if _, err := c.Do(http.MethodPut, c.kvBase()+"/bulk", body); err != nil {
			return err
		}
		batch = batch[:0]
		size = 0
		return nil
	}

	for _, p := range pairs {
		if len(batch) >= maxBulkKeys || size+len(p.Value) > maxBulkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, p)
		size += len(p.Value)
	}
	return flush()
}

func (c *Client) BulkDelete(names []string) error {
	body, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("cannot encode delete: %w", err)
	}
	_, err = c.Do(http.MethodPost, c.kvBase()+"/bulk/delete", body)
	return err
}

func (c *Client) ListKeys(prefix string) ([]Key, error) {
	var keys []Key
	cursor := ""
	for {
		q := url.Values{}
		q.Set("prefix", prefix)
		q.Set("limit", "1000")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := c.Do(http.MethodGet, c.kvBase()+"/keys?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page []Key
		if err := json.Unmarshal(resp.Result, &page); err != nil {
			return nil, fmt.Errorf("unexpected key listing: %w", err)
		}
		keys = append(keys, page...)
		cursor = resp.ResultInfo.Cursor
		if cursor == "" {
			return keys, nil
		}
	}
}

// Value fetches one key's raw bytes. The bulk API cannot read, so a rename copies
// object by object.
func (c *Client) Value(key string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, apiBase+c.kvBase()+"/values/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, fmt.Errorf("bad request for %s: %w", key, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s failed: %w", key, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s failed: %w", key, err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API %d reading %s: %s", res.StatusCode, key, truncate(string(raw), 200))
	}
	return raw, nil
}

// MetaString and MetaNumber read the loosely-typed metadata KV hands back.
func MetaString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func MetaNumber(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}
