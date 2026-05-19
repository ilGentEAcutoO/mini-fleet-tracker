package cfclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// ---------------------------------------------------------------------------
// ListObjects — exercises the real aws-sdk-go-v2 wire path against an
// httptest.Server that responds with canned ListBucketResult XML. We can't
// reuse newR2TestClient here because that points at a fake hostname; the
// SDK would refuse to talk to it. So we build a dedicated httptest-backed
// client per test, which also lets each test assert on the request URL and
// query params the SDK actually sent.
// ---------------------------------------------------------------------------

// newR2ListTestClient builds an R2Client whose endpoint points at the
// provided httptest server. The server's handler should respond with a
// ListBucketResult XML envelope for ListObjectsV2 to parse.
func newR2ListTestClient(t *testing.T, srv *httptest.Server) *R2Client {
	t.Helper()
	c, err := NewR2Client(context.Background(), R2Config{
		Endpoint:        srv.URL,
		AccessKeyID:     "AKIA-TEST",
		SecretAccessKey: "secret-test",
		BucketName:      "mft-r2-photos",
	})
	if err != nil {
		t.Fatalf("NewR2Client: %v", err)
	}
	return c
}

func TestR2_ListObjects_HappyPath(t *testing.T) {
	var gotMethod, gotPath, gotPrefix, gotMaxKeys string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotPrefix = r.URL.Query().Get("prefix")
		gotMaxKeys = r.URL.Query().Get("max-keys")
		// Canonical ListBucketResult v2 envelope — three keys under the
		// requested prefix. The SDK parses xmlns="http://s3.amazonaws.com/doc/2006-03-01/"
		// even though R2 also accepts the namespace-less form.
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>mft-r2-photos</Name>
  <Prefix>vehicles/veh-1/</Prefix>
  <KeyCount>3</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>vehicles/veh-1/a.jpg</Key>
    <LastModified>2026-05-19T00:00:00.000Z</LastModified>
    <ETag>"deadbeef"</ETag>
    <Size>1024</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
  <Contents>
    <Key>vehicles/veh-1/b.jpg</Key>
    <LastModified>2026-05-19T00:01:00.000Z</LastModified>
    <ETag>"deadbeef"</ETag>
    <Size>2048</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
  <Contents>
    <Key>vehicles/veh-1/c.jpg</Key>
    <LastModified>2026-05-19T00:02:00.000Z</LastModified>
    <ETag>"deadbeef"</ETag>
    <Size>4096</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
</ListBucketResult>`)
	}))
	t.Cleanup(srv.Close)

	c := newR2ListTestClient(t, srv)
	keys, err := c.ListObjects(context.Background(), "vehicles/veh-1/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3 (keys=%v)", len(keys), keys)
	}
	want := []string{"vehicles/veh-1/a.jpg", "vehicles/veh-1/b.jpg", "vehicles/veh-1/c.jpg"}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], k)
		}
	}
	// Wire-level assertions: the SDK should send GET /<bucket>/?list-type=2&prefix=...&max-keys=1000.
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if !strings.Contains(gotPath, "/mft-r2-photos") {
		t.Errorf("path = %q, want bucket in path", gotPath)
	}
	if gotPrefix != "vehicles/veh-1/" {
		t.Errorf("prefix query = %q, want vehicles/veh-1/", gotPrefix)
	}
	if gotMaxKeys != "1000" {
		t.Errorf("max-keys query = %q, want 1000", gotMaxKeys)
	}
}

func TestR2_ListObjects_EmptyPrefixOmitsParam(t *testing.T) {
	var gotPrefix string
	var prefixPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, prefixPresent = r.URL.Query()["prefix"]
		gotPrefix = r.URL.Query().Get("prefix")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <KeyCount>0</KeyCount>
  <IsTruncated>false</IsTruncated>
</ListBucketResult>`)
	}))
	t.Cleanup(srv.Close)

	c := newR2ListTestClient(t, srv)
	keys, err := c.ListObjects(context.Background(), "   ")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("len(keys) = %d, want 0 (no contents)", len(keys))
	}
	if prefixPresent && gotPrefix != "" {
		t.Errorf("prefix param should be omitted on blank input, got %q", gotPrefix)
	}
}

func TestR2_ListObjects_NoContents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A list with no <Contents> elements — typical for an empty prefix.
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>mft-r2-photos</Name>
  <Prefix>vehicles/empty/</Prefix>
  <KeyCount>0</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
</ListBucketResult>`)
	}))
	t.Cleanup(srv.Close)

	c := newR2ListTestClient(t, srv)
	keys, err := c.ListObjects(context.Background(), "vehicles/empty/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	// Non-nil empty slice — required by the doc-comment contract so
	// callers can `for _, k := range keys` without a nil-guard.
	if keys == nil {
		t.Error("expected non-nil empty slice on no-contents response")
	}
	if len(keys) != 0 {
		t.Errorf("len(keys) = %d, want 0", len(keys))
	}
}

func TestR2_ListObjects_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// AWS-style XML error so the SDK's error parser has something to chew on.
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InternalError</Code><Message>boom</Message></Error>`)
	}))
	t.Cleanup(srv.Close)

	c := newR2ListTestClient(t, srv)
	_, err := c.ListObjects(context.Background(), "vehicles/veh-1/")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "cfclient.R2.ListObjects") {
		t.Errorf("error should be wrapped with op name: %v", err)
	}
}
