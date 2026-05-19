// Package cfclient provides typed HTTP clients for the Cloudflare services
// the Mini Fleet Tracker backend depends on: D1 (SQLite over HTTP), KV
// (key-value with TTL), R2 (S3-compatible blob storage), and a Durable
// Object publish endpoint over an HMAC-signed POST.
//
// Each client takes an *http.Client so tests can inject a transport pointed
// at httptest.Server; production wiring uses a default client with a
// caller-supplied timeout.
package cfclient
