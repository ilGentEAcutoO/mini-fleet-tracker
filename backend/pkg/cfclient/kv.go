package cfclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KVClient talks to a single Cloudflare Workers KV namespace via the REST
// API. One client targets one namespace; create multiple clients for the
// sessions / ratelimits / quotas namespaces this project uses.
type KVClient struct {
	accountID   string
	namespaceID string
	apiToken    string
	httpClient  *http.Client
	baseURL     string
}

// KVConfig is the constructor input for NewKVClient. Mirrors D1Config so
// callers can wire all CF clients the same way.
type KVConfig struct {
	AccountID   string
	NamespaceID string
	APIToken    string
	HTTPClient  *http.Client
	BaseURL     string
}

// NewKVClient validates cfg and returns a ready-to-use client.
func NewKVClient(cfg KVConfig) (*KVClient, error) {
	if strings.TrimSpace(cfg.AccountID) == "" {
		return nil, errors.New("cfclient.KV: AccountID is required")
	}
	if strings.TrimSpace(cfg.NamespaceID) == "" {
		return nil, errors.New("cfclient.KV: NamespaceID is required")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, errors.New("cfclient.KV: APIToken is required")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}

	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.cloudflare.com"
	}

	return &KVClient{
		accountID:   cfg.AccountID,
		namespaceID: cfg.NamespaceID,
		apiToken:    cfg.APIToken,
		httpClient:  hc,
		baseURL:     base,
	}, nil
}

// kvMinTTL is Cloudflare's enforced floor for expiration_ttl. Anything
// strictly between zero and this value is rounded up at the client to keep
// the surface predictable; zero stays zero (no expiration).
const kvMinTTL = 60 * time.Second

// valueURL returns the URL for the values/{key} endpoint.
//
// The key is interpolated into the URL path. CF KV permits a wide range of
// printable characters in keys, but we still encode each path segment so
// keys containing "/", "?", "#", or whitespace round-trip safely.
func (c *KVClient) valueURL(key string) string {
	return fmt.Sprintf(
		"%s/client/v4/accounts/%s/storage/kv/namespaces/%s/values/%s",
		c.baseURL, c.accountID, c.namespaceID, url.PathEscape(key),
	)
}

// Get fetches a single value. A missing key returns (nil, false, nil) — the
// (found bool) idiom lets callers distinguish "key absent" from "value is
// the empty byte slice", which the byte-slice return alone cannot express.
func (c *KVClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if strings.TrimSpace(key) == "" {
		return nil, false, errors.New("cfclient.KV.Get: key is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.valueURL(key), nil)
	if err != nil {
		return nil, false, fmt.Errorf("kv get: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("kv get: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("kv get: read body: %w", err)
		}
		return body, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, false, fmt.Errorf("kv get: %w (status %d): %s",
			ErrUnauthorized, resp.StatusCode, readShort(resp.Body))
	default:
		return nil, false, fmt.Errorf("kv get: http %d: %s",
			resp.StatusCode, readShort(resp.Body))
	}
}

// Put writes value with optional TTL. A zero TTL means "no expiration".
// A non-zero TTL below kvMinTTL is rounded up to kvMinTTL — CF KV rejects
// shorter values, and silently rounding is friendlier to callers than
// surfacing the limit at every call site.
func (c *KVClient) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("cfclient.KV.Put: key is required")
	}

	u := c.valueURL(key)
	if ttl > 0 {
		if ttl < kvMinTTL {
			ttl = kvMinTTL
		}
		seconds := int64(ttl / time.Second)
		u = fmt.Sprintf("%s?expiration_ttl=%d", u, seconds)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("kv put: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kv put: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("kv put: %w (status %d): %s",
			ErrUnauthorized, resp.StatusCode, readShort(resp.Body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kv put: http %d: %s", resp.StatusCode, readShort(resp.Body))
	}
	return nil
}

// Delete removes a key. A 404 is folded into a nil return because "key
// gone" and "key was never there" are indistinguishable to the caller and
// almost always the same business outcome (e.g. logout cleanup).
func (c *KVClient) Delete(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("cfclient.KV.Delete: key is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.valueURL(key), nil)
	if err != nil {
		return fmt.Errorf("kv delete: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kv delete: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("kv delete: %w (status %d): %s",
			ErrUnauthorized, resp.StatusCode, readShort(resp.Body))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kv delete: http %d: %s", resp.StatusCode, readShort(resp.Body))
	}
	return nil
}

// readShort drains up to 512 bytes of a response body and returns it
// trimmed. Used purely to embed a useful snippet in error messages without
// risking unbounded reads of HTML error pages.
func readShort(r io.Reader) string {
	const maxBytes = 512
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(io.LimitReader(r, maxBytes), buf)
	s := strings.TrimSpace(string(buf[:n]))
	if s == "" {
		return "<empty response>"
	}
	return s
}

