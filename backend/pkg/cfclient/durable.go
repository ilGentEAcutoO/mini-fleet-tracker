package cfclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DurableClient posts JSON events to the gateway Worker's
// /internal/publish endpoint. The endpoint is fronted by a shared-secret
// HMAC so only the Go API can publish — public WebSocket clients receive
// what the DO broadcasts but cannot publish themselves.
type DurableClient struct {
	publishURL string
	secret     []byte
	httpClient *http.Client
}

// DurableConfig is the constructor input for NewDurableClient.
type DurableConfig struct {
	// PublishURL is the absolute URL of the DO publish endpoint. In dev
	// this is typically `http://localhost:8787/internal/publish`; in prod
	// it points at the deployed gateway Worker.
	PublishURL string
	// Secret is the shared HMAC key (INTERNAL_PUBLISH_SECRET in config).
	Secret string
	// HTTPClient is optional; when nil a client with a 5s timeout is used.
	HTTPClient *http.Client
}

// NewDurableClient validates cfg and returns a ready-to-use client.
func NewDurableClient(cfg DurableConfig) (*DurableClient, error) {
	if strings.TrimSpace(cfg.PublishURL) == "" {
		return nil, errors.New("cfclient.Durable: PublishURL is required")
	}
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, errors.New("cfclient.Durable: Secret is required")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}

	return &DurableClient{
		publishURL: cfg.PublishURL,
		secret:     []byte(cfg.Secret),
		httpClient: hc,
	}, nil
}

// Publish JSON-encodes event, computes HMAC-SHA256(body, secret), and
// POSTs the body with the hex digest in X-Signature. The DO receiver
// verifies the signature byte-for-byte against the same secret and
// rejects anything mismatched, so any wire-level tampering or accidental
// re-use of the wrong secret fails closed.
func (c *DurableClient) Publish(ctx context.Context, event any) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("durable publish: marshal event: %w", err)
	}

	mac := hmac.New(sha256.New, c.secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.publishURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("durable publish: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("durable publish: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("durable publish: %w (status %d): %s",
			ErrUnauthorized, resp.StatusCode, readShort(resp.Body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("durable publish: http %d: %s",
			resp.StatusCode, readShort(resp.Body))
	}
	return nil
}
