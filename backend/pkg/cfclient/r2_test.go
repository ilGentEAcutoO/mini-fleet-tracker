package cfclient

import (
	"context"
	"strings"
	"testing"
	"time"
)

// R2 testing strategy: per TASK-005 brief, we picked the "lighter" pattern
// — call the presigner with a realistic-looking endpoint and assert on the
// returned URL's shape (scheme, host, path, SigV4 query params, expires).
// We do NOT validate the SigV4 signature byte-for-byte; that's the SDK's
// job, and re-implementing the algorithm here just to assert equality
// would defeat the point of using the SDK. A round-trip integration test
// against a real R2 bucket lives outside this package (TASK-022).

func newR2TestClient(t *testing.T) *R2Client {
	t.Helper()
	c, err := NewR2Client(context.Background(), R2Config{
		Endpoint:        "https://acc-test.r2.cloudflarestorage.com",
		AccessKeyID:     "AKIA-TEST",
		SecretAccessKey: "secret-test",
		BucketName:      "mft-r2-photos",
	})
	if err != nil {
		t.Fatalf("NewR2Client: %v", err)
	}
	return c
}

func TestNewR2Client_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  R2Config
		want string
	}{
		{
			"missing Endpoint",
			R2Config{AccessKeyID: "a", SecretAccessKey: "s", BucketName: "b"},
			"Endpoint",
		},
		{
			"missing AccessKeyID",
			R2Config{Endpoint: "https://x", SecretAccessKey: "s", BucketName: "b"},
			"AccessKeyID",
		},
		{
			"missing SecretAccessKey",
			R2Config{Endpoint: "https://x", AccessKeyID: "a", BucketName: "b"},
			"SecretAccessKey",
		},
		{
			"missing BucketName",
			R2Config{Endpoint: "https://x", AccessKeyID: "a", SecretAccessKey: "s"},
			"BucketName",
		},
		{
			"endpoint missing scheme",
			R2Config{Endpoint: "no-scheme", AccessKeyID: "a", SecretAccessKey: "s", BucketName: "b"},
			"scheme",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewR2Client(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestR2_PresignGetObject_HasSigV4Params(t *testing.T) {
	c := newR2TestClient(t)
	u, err := c.PresignGetObject(context.Background(), "photos/veh-1/2026/05/19.jpg", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q", u.Scheme)
	}
	if u.Host != "acc-test.r2.cloudflarestorage.com" {
		t.Errorf("host = %q", u.Host)
	}
	// Path-style: /<bucket>/<key>
	if !strings.Contains(u.Path, "/mft-r2-photos/photos/veh-1/2026/05/19.jpg") {
		t.Errorf("path = %q (want /<bucket>/<key>)", u.Path)
	}

	q := u.Query()
	requiredParams := []string{
		"X-Amz-Algorithm",
		"X-Amz-Credential",
		"X-Amz-Date",
		"X-Amz-Expires",
		"X-Amz-SignedHeaders",
		"X-Amz-Signature",
	}
	for _, p := range requiredParams {
		if q.Get(p) == "" {
			t.Errorf("missing required SigV4 param %q", p)
		}
	}
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("algorithm = %q", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Errorf("expires = %q, want 300 (5 minutes)", q.Get("X-Amz-Expires"))
	}
}

func TestR2_PresignPutObject_HasSigV4Params(t *testing.T) {
	c := newR2TestClient(t)
	u, hdr, err := c.PresignPutObject(context.Background(), "uploads/abc.jpg", 10*time.Minute, 0)
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}
	if u.Host != "acc-test.r2.cloudflarestorage.com" {
		t.Errorf("host = %q", u.Host)
	}
	if !strings.Contains(u.Path, "/mft-r2-photos/uploads/abc.jpg") {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "600" {
		t.Errorf("expires = %q, want 600", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("missing signature")
	}
	// hdr may be empty or contain Host depending on SDK version; we just
	// want a non-nil map so the caller can range over it.
	if hdr == nil {
		t.Error("signed-header map should be non-nil even when empty")
	}
}

func TestR2_PresignPutObject_WithContentLengthCap(t *testing.T) {
	c := newR2TestClient(t)
	const maxBytes = int64(5 * 1024 * 1024) // 5 MiB
	_, hdr, err := c.PresignPutObject(context.Background(), "uploads/big.jpg", 5*time.Minute, maxBytes)
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}
	// When ContentLength is set on PutObjectInput the SDK signs Content-Length
	// and surfaces it in SignedHeader so the uploader knows it must send
	// exactly that value. Verify it propagates.
	if got := hdr.Get("Content-Length"); got == "" {
		t.Errorf("expected Content-Length in signed headers when maxBytes provided, got headers: %+v", hdr)
	}
}

func TestR2_PresignGetObject_RejectsEmptyKey(t *testing.T) {
	c := newR2TestClient(t)
	if _, err := c.PresignGetObject(context.Background(), " ", time.Minute); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestR2_PresignGetObject_RejectsZeroTTL(t *testing.T) {
	c := newR2TestClient(t)
	if _, err := c.PresignGetObject(context.Background(), "k", 0); err == nil {
		t.Fatal("expected error for zero TTL")
	}
}

func TestR2_PresignPutObject_RejectsEmptyKey(t *testing.T) {
	c := newR2TestClient(t)
	if _, _, err := c.PresignPutObject(context.Background(), " ", time.Minute, 0); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestR2_PresignPutObject_RejectsZeroTTL(t *testing.T) {
	c := newR2TestClient(t)
	if _, _, err := c.PresignPutObject(context.Background(), "k", 0, 0); err == nil {
		t.Fatal("expected error for zero TTL")
	}
}

func TestR2_DefaultsRegionAuto(t *testing.T) {
	// Omitting Region in R2Config should fall back to "auto" without
	// failing the constructor. We can't easily inspect the SDK's internal
	// region after creation, but we can at least verify the client is
	// usable.
	c, err := NewR2Client(context.Background(), R2Config{
		Endpoint:        "https://acc-test.r2.cloudflarestorage.com",
		AccessKeyID:     "a",
		SecretAccessKey: "s",
		BucketName:      "b",
		// Region intentionally omitted.
	})
	if err != nil {
		t.Fatalf("NewR2Client without Region: %v", err)
	}
	if _, err := c.PresignGetObject(context.Background(), "k", time.Minute); err != nil {
		t.Fatalf("PresignGetObject with default region: %v", err)
	}
}

func TestR2_BadEndpointURL(t *testing.T) {
	_, err := NewR2Client(context.Background(), R2Config{
		Endpoint:        "://broken",
		AccessKeyID:     "a",
		SecretAccessKey: "s",
		BucketName:      "b",
	})
	if err == nil {
		t.Fatal("expected error for malformed endpoint")
	}
}
