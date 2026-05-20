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
	"strconv"
	"strings"
	"time"
)

// DurableClient posts JSON events to the gateway Worker's
// /internal/publish endpoint. The endpoint is fronted by a shared-secret
// HMAC so only the Go API can publish — public WebSocket clients receive
// what the DO broadcasts but cannot publish themselves.
//
// TASK-051 / security review H2: the wire envelope now binds a Unix-seconds
// timestamp into the signature input so captured signed requests cannot
// be replayed indefinitely. The verifier (gateway + DO) accepts only
// `HMAC-SHA256(body || '\n' || ts, secret)` paired with an X-Timestamp
// header carrying the same `ts`, within a ±30s window. During the
// 24h rollout the verifier also accepts the legacy `HMAC-SHA256(body,
// secret)` so deploys can land in any order without breaking
// broadcasts; once the legacy log line drains, that fallback is
// removed at the verifier end.
type DurableClient struct {
	publishURL string
	secret     []byte
	httpClient *http.Client
	// nowSec is the testable clock. Production leaves it nil so the
	// constructor wires time.Now-based behaviour; tests pin it to a
	// fixed instant when they need deterministic signatures (so far
	// no test needs that — the round-trip tests sleep + assert
	// timestamps differ, which is robust against clock skew).
	nowSec func() int64
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

// Publish JSON-encodes event, computes HMAC-SHA256(body || '\n' || ts,
// secret), and POSTs the body with the hex digest in X-Signature plus
// the timestamp in X-Timestamp. The DO + gateway verifier accept the
// signature only when |now - ts| <= 30s AND the signature matches the
// new composition; otherwise the request is rejected with 401. During
// the 24h rollout the verifiers also accept the legacy body-only HMAC
// so out-of-order deploys do not drop events.
//
// Byte-exact wire contract (TASK-051):
//   - X-Signature header: lowercase hex of HMAC-SHA256 output.
//   - X-Timestamp header: decimal-ASCII Unix seconds, e.g. "1721050923".
//   - HMAC input: body bytes, then a single LF (0x0a), then the
//     UTF-8 bytes of the timestamp string. NOT the integer value;
//     NOT trailing whitespace.
//
// Both ends of the wire (Go publisher, gateway verifier, DO verifier)
// MUST agree on this exact composition or the verifier silently
// rejects all events. The mirroring test computeHMACNew in
// durable_test.go pins the bytes — change either side and the test
// trips before deploy.
func (c *DurableClient) Publish(ctx context.Context, event any) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("durable publish: marshal event: %w", err)
	}

	// Unix-seconds (10-digit decimal string). The verifier parses with
	// JS's `parseInt`, which is happy with leading-zero-free decimal —
	// strconv.FormatInt produces exactly that.
	now := c.nowSec
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	ts := strconv.FormatInt(now(), 10)

	// HMAC input = body || '\n' || ts. Built in a single buffer so the
	// signature is computed off a contiguous slice — keeps the mental
	// model identical to the JS verifier's `new TextEncoder().encode(
	// body+'\n'+ts)` and avoids any chance of writing the wrong number
	// of bytes through hmac.Write.
	signedInput := make([]byte, 0, len(body)+1+len(ts))
	signedInput = append(signedInput, body...)
	signedInput = append(signedInput, '\n')
	signedInput = append(signedInput, []byte(ts)...)

	mac := hmac.New(sha256.New, c.secret)
	mac.Write(signedInput)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.publishURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("durable publish: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", sig)
	req.Header.Set("X-Timestamp", ts)

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
