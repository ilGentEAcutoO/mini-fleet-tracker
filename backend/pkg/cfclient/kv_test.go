package cfclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newKVTestClient(t *testing.T, handler http.HandlerFunc) *KVClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewKVClient(KVConfig{
		AccountID:   "acc-test",
		NamespaceID: "ns-test",
		APIToken:    "tok-test",
		BaseURL:     srv.URL,
	})
	if err != nil {
		t.Fatalf("NewKVClient: %v", err)
	}
	return c
}

func TestNewKVClient_RequiredFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  KVConfig
		want string
	}{
		{"missing AccountID", KVConfig{NamespaceID: "n", APIToken: "t"}, "AccountID"},
		{"missing NamespaceID", KVConfig{AccountID: "a", APIToken: "t"}, "NamespaceID"},
		{"missing APIToken", KVConfig{AccountID: "a", NamespaceID: "n"}, "APIToken"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewKVClient(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestKV_Get_Found(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	c := newKVTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = io.WriteString(w, "raw-value-bytes")
	})

	v, found, err := c.Get(context.Background(), "session:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if string(v) != "raw-value-bytes" {
		t.Fatalf("value = %q", v)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q", gotMethod)
	}
	if gotAuth != "Bearer tok-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	// PathEscape on "session:abc" leaves the colon intact (RFC 3986 says
	// ':' is a reserved sub-delim that PathEscape doesn't touch); the key
	// segment of the path must still be present.
	if !strings.HasSuffix(gotPath,
		"/client/v4/accounts/acc-test/storage/kv/namespaces/ns-test/values/session:abc") {
		t.Errorf("path = %q", gotPath)
	}
}

func TestKV_Get_NotFound(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":10009,"message":"key not found"}]}`)
	})

	v, found, err := c.Get(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Fatal("expected found=false")
	}
	if v != nil {
		t.Fatalf("expected nil value, got %v", v)
	}
}

func TestKV_Get_Unauthorized(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "bad token")
	})
	_, _, err := c.Get(context.Background(), "k")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestKV_Get_EmptyKey(t *testing.T) {
	c, _ := NewKVClient(KVConfig{AccountID: "a", NamespaceID: "n", APIToken: "t"})
	_, _, err := c.Get(context.Background(), " ")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestKV_Put_SendsBodyAndTTL(t *testing.T) {
	var (
		gotAuth   string
		gotMethod string
		gotPath   string
		gotQuery  string
		gotCT     string
		gotBody   []byte
	)
	c := newKVTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	if err := c.Put(context.Background(), "session:abc",
		[]byte(`{"uid":"drv-1"}`), 5*time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q", gotMethod)
	}
	if gotAuth != "Bearer tok-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if !strings.HasSuffix(gotPath,
		"/client/v4/accounts/acc-test/storage/kv/namespaces/ns-test/values/session:abc") {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "expiration_ttl=300" {
		t.Errorf("query = %q", gotQuery)
	}
	if string(gotBody) != `{"uid":"drv-1"}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestKV_Put_ZeroTTL_NoExpirationQuery(t *testing.T) {
	var gotQuery string
	c := newKVTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	if err := c.Put(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("expected no query for zero TTL, got %q", gotQuery)
	}
}

func TestKV_Put_RoundsUpBelowMinTTL(t *testing.T) {
	var gotQuery string
	c := newKVTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	// 10s is under CF's 60s floor — client should round up silently.
	if err := c.Put(context.Background(), "k", []byte("v"), 10*time.Second); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotQuery != "expiration_ttl=60" {
		t.Fatalf("expected expiration_ttl=60, got %q", gotQuery)
	}
}

func TestKV_Put_Unauthorized(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "denied")
	})
	err := c.Put(context.Background(), "k", []byte("v"), 0)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestKV_Put_BadStatus(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad")
	})
	err := c.Put(context.Background(), "k", []byte("v"), 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("400 should not surface as ErrUnauthorized: %v", err)
	}
}

func TestKV_Put_EmptyKey(t *testing.T) {
	c, _ := NewKVClient(KVConfig{AccountID: "a", NamespaceID: "n", APIToken: "t"})
	if err := c.Put(context.Background(), " ", []byte("v"), 0); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestKV_Delete_OK(t *testing.T) {
	var gotMethod, gotAuth string
	c := newKVTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	if err := c.Delete(context.Background(), "session:abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q", gotMethod)
	}
	if gotAuth != "Bearer tok-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestKV_Delete_404IsNotError(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("404 on delete should not error: %v", err)
	}
}

func TestKV_Delete_Unauthorized(t *testing.T) {
	c := newKVTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	err := c.Delete(context.Background(), "k")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestKV_Delete_EmptyKey(t *testing.T) {
	c, _ := NewKVClient(KVConfig{AccountID: "a", NamespaceID: "n", APIToken: "t"})
	if err := c.Delete(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
}
