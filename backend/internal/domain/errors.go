// Package domain defines the core entities, value objects, and sentinel
// errors of the Mini Fleet Tracker business model.
//
// Errors in this package are exported as plain *errors.errorString sentinels
// so callers can wrap them with context (`fmt.Errorf("driver email %s: %w",
// email, domain.ErrAlreadyExists)`) and detect them at the boundary with
// `errors.Is`. The HTTP layer maps each sentinel onto a concrete status
// code — keep the set small and stable so that mapping stays trivial.
//
// Convention: error messages are lower-case and have no trailing period,
// matching the Go standard library style.
package domain

import "errors"

// Sentinel errors used across the domain, repository, and usecase layers.
// The set is intentionally minimal so the HTTP error-mapping middleware can
// stay tabular.
var (
	// ErrNotFound is returned when a lookup yields no row. The HTTP layer
	// maps this onto 404 Not Found.
	ErrNotFound = errors.New("not found")

	// ErrAlreadyExists is returned when a uniqueness invariant is violated
	// (e.g. duplicate driver email). The HTTP layer maps this onto 409
	// Conflict.
	ErrAlreadyExists = errors.New("already exists")

	// ErrUnauthorized is returned when credentials are missing or invalid.
	// Critically, Login returns this for *both* "unknown email" and "wrong
	// password" so the API cannot be used as a user-enumeration oracle. The
	// HTTP layer maps it onto 401 Unauthorized.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrForbidden is returned when the caller is authenticated but lacks
	// permission for the requested action (e.g. a driver editing another
	// driver's vehicle). The HTTP layer maps this onto 403 Forbidden.
	ErrForbidden = errors.New("forbidden")

	// ErrValidation is returned for malformed input: missing required
	// fields, bad enum values, etc. The HTTP layer maps this onto 400
	// Bad Request and may include a per-field validation report.
	ErrValidation = errors.New("validation failed")

	// ErrTooMany is returned when a rate-limit or quota guardrail trips.
	// The HTTP layer maps this onto 429 Too Many Requests.
	ErrTooMany = errors.New("too many requests")
)
