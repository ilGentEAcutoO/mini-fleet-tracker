// Package handler contains the Fiber HTTP handlers that translate inbound
// requests into usecase calls and shape responses into JSON. Each handler
// is a method on a small struct holding its dependencies; the wiring layer
// (cmd/api) is the only place that instantiates them.
//
// Conventions:
//   - request structs use struct tags from go-playground/validator/v10
//   - response structs never embed domain types directly — they use
//     hand-written DTOs that strip sensitive fields (password_hash being
//     the obvious case)
//   - errors return via the shared respondError helper which standardises
//     the envelope { error, message, request_id }
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/pkg/jwt"
)

// AuthUsecase is the narrow contract the auth handler needs from the
// usecase layer. Declared at the consumer site so the handler does not
// need to import the concrete usecase struct (and so tests can mock with
// minimal surface area).
type AuthUsecase interface {
	Register(ctx context.Context, email, password, name string, role domain.Role) (*domain.Driver, error)
	Login(ctx context.Context, email, password string) (string, *domain.Driver, error)
	Me(ctx context.Context, userID string) (*domain.Driver, error)
	Logout(ctx context.Context, claims *jwt.Claims) error
}

// TokenIntrospector is the narrow contract the handler needs from the
// JWT signer — only the Remaining method is used, by Login when sizing
// the cookie MaxAge to match the token TTL. The real *jwt.Signer satisfies
// this directly.
type TokenIntrospector interface {
	Remaining(c *jwt.Claims) time.Duration
	Verify(token string) (*jwt.Claims, error)
}

// CookieAttrs captures the per-environment cookie attributes. Production
// sets Secure=true and Domain to the public host so the browser scopes
// the cookie to fleet-tracker.jairuchan.com; development leaves Secure=
// false and Domain empty so plain HTTP localhost works without TLS.
//
// SameSite is fixed to Lax in both environments — Strict would break the
// CSRF cookie's intended dual-submit pattern (the cookie needs to ride
// along on legitimate same-site fetches), and None would weaken
// cross-origin protection without a corresponding security benefit for
// our single-origin SPA.
type CookieAttrs struct {
	Secure   bool
	Domain   string
	Path     string
	SameSite string
}

// DefaultCookieAttrs returns sane defaults for dev or prod. Wiring code
// in main.go calls this to avoid hand-typing the same struct in two
// places.
func DefaultCookieAttrs(isDevelopment bool) CookieAttrs {
	if isDevelopment {
		return CookieAttrs{
			Secure:   false,
			Domain:   "",
			Path:     "/",
			SameSite: "Lax",
		}
	}
	return CookieAttrs{
		Secure: true,
		// Empty Domain is deliberate: it scopes the cookie to the host
		// that issued it (fleet-tracker.jairuchan.com), not the apex
		// (.jairuchan.com). That matches the demo's intent — only the
		// fleet tracker SPA should receive these cookies, not any other
		// app on the parent domain.
		Domain:   "",
		Path:     "/",
		SameSite: "Lax",
	}
}

// AuthHandler is the HTTP-facing facade for the auth workflows.
type AuthHandler struct {
	usecase     AuthUsecase
	signer      TokenIntrospector
	cookieAttrs CookieAttrs
	validate    *validator.Validate
}

// NewAuthHandler validates its inputs and returns a ready-to-route handler.
// Returns an error (rather than panicking) so the wiring layer can log a
// structured boot failure.
func NewAuthHandler(usecase AuthUsecase, signer TokenIntrospector, attrs CookieAttrs) (*AuthHandler, error) {
	if usecase == nil {
		return nil, errors.New("auth handler: usecase is required")
	}
	if signer == nil {
		return nil, errors.New("auth handler: signer is required")
	}
	if strings.TrimSpace(attrs.Path) == "" {
		attrs.Path = "/"
	}
	if strings.TrimSpace(attrs.SameSite) == "" {
		attrs.SameSite = "Lax"
	}
	return &AuthHandler{
		usecase:     usecase,
		signer:      signer,
		cookieAttrs: attrs,
		validate:    validator.New(validator.WithRequiredStructEnabled()),
	}, nil
}

// ---------------------------------------------------------------------------
// Request / response shapes.
// ---------------------------------------------------------------------------

// registerRequest is the POST /api/auth/register input. The min=8/max=72
// password bounds match Argon2id's working range — 72 is also bcrypt's
// historical limit, kept here so future migration off argon2id does not
// silently truncate existing passwords.
type registerRequest struct {
	Email    string `json:"email"    validate:"required,email,max=320"`
	Password string `json:"password" validate:"required,min=8,max=72"`
	Name     string `json:"name"     validate:"required,min=1,max=200"`
	Role     string `json:"role"     validate:"required,oneof=driver manager"`
}

// loginRequest is the POST /api/auth/login input.
type loginRequest struct {
	Email    string `json:"email"    validate:"required,email,max=320"`
	Password string `json:"password" validate:"required,min=8,max=72"`
}

// driverDTO is the public projection of domain.Driver: password_hash is
// dropped entirely so it cannot leak through a misconfigured logger or
// a schema-shifting encoder. Timestamps stay in unix-millis because the
// SPA does the formatting and that's also what the D1 schema stores.
type driverDTO struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// toDTO copies the safe-to-serialise fields out of a domain.Driver.
// Nil-safe: returns the zero DTO when d is nil so callers do not need
// to nil-check before encoding.
func toDTO(d *domain.Driver) driverDTO {
	if d == nil {
		return driverDTO{}
	}
	return driverDTO{
		ID:        d.ID,
		Email:     d.Email,
		Name:      d.Name,
		Role:      string(d.Role),
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
}

// errorBody is the standard JSON envelope for every non-2xx response from
// this package. The request_id field is populated from the request scope
// so operators can correlate failed requests with the access log.
type errorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// userBody is the standard 2xx envelope when a single driver is returned.
type userBody struct {
	User driverDTO `json:"user"`
}

// statusBody is the standard ack envelope for endpoints that return only
// success/failure (logout being the canonical example).
type statusBody struct {
	Status string `json:"status"`
}

// ---------------------------------------------------------------------------
// Handler methods.
// ---------------------------------------------------------------------------

// Register creates a new driver. POST /api/auth/register, no auth required.
//
// 201 on success, 400 on validation failure, 409 on duplicate email,
// 500 on infrastructure failure.
func (h *AuthHandler) Register(c *fiber.Ctx) error {
	var req registerRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	d, err := h.usecase.Register(c.UserContext(), req.Email, req.Password, req.Name, domain.Role(req.Role))
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrAlreadyExists):
			return h.respondError(c, http.StatusConflict, "already_exists", "email already registered")
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not register driver")
		}
	}
	return c.Status(http.StatusCreated).JSON(userBody{User: toDTO(d)})
}

// Login verifies credentials and sets the auth + csrf cookies. POST
// /api/auth/login, no auth required.
//
// 200 on success (with two Set-Cookie headers), 400 on validation, 401
// on bad credentials, 500 on infrastructure failure.
func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	token, d, err := h.usecase.Login(c.UserContext(), req.Email, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrUnauthorized):
			return h.respondError(c, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not login")
		}
	}

	// Verify our own freshly-issued token so we can read its Claims and
	// compute the cookie MaxAge from the remaining TTL. This avoids a
	// hard-coded constant and means a future TTL change in the signer
	// flows through to the cookie automatically.
	claims, err := h.signer.Verify(token)
	if err != nil {
		return h.respondError(c, http.StatusInternalServerError, "internal", "could not finalise session")
	}
	maxAge := int(h.signer.Remaining(claims).Seconds())
	if maxAge <= 0 {
		// Defensive: a non-positive MaxAge would either omit the
		// attribute or evict the cookie immediately. Either case is
		// indistinguishable from a server bug — surface it.
		return h.respondError(c, http.StatusInternalServerError, "internal", "could not finalise session")
	}

	// Mint a fresh CSRF token bound to this session. 32 bytes of randomness
	// rendered as hex (64 chars) — comfortably above the 16-byte floor
	// recommended for CSRF tokens, and short enough that the cookie size
	// is not an issue.
	csrf, err := generateCSRFToken()
	if err != nil {
		return h.respondError(c, http.StatusInternalServerError, "internal", "could not generate csrf token")
	}

	// Set both cookies. The auth_token cookie is HttpOnly (so document.cookie
	// cannot exfiltrate it); the csrf_token cookie is NOT HttpOnly because
	// the SPA needs to read it and copy the value to the X-CSRF-Token header
	// on every mutating fetch (double-submit cookie pattern).
	expires := time.Now().Add(time.Duration(maxAge) * time.Second)
	c.Cookie(&fiber.Cookie{
		Name:     middleware.AuthCookieName,
		Value:    token,
		Path:     h.cookieAttrs.Path,
		Domain:   h.cookieAttrs.Domain,
		MaxAge:   maxAge,
		Expires:  expires,
		Secure:   h.cookieAttrs.Secure,
		HTTPOnly: true,
		SameSite: h.cookieAttrs.SameSite,
	})
	c.Cookie(&fiber.Cookie{
		Name:     middleware.CSRFCookieName,
		Value:    csrf,
		Path:     h.cookieAttrs.Path,
		Domain:   h.cookieAttrs.Domain,
		MaxAge:   maxAge,
		Expires:  expires,
		Secure:   h.cookieAttrs.Secure,
		HTTPOnly: false,
		SameSite: h.cookieAttrs.SameSite,
	})
	return c.JSON(userBody{User: toDTO(d)})
}

// Logout clears the auth + csrf cookies and adds the token's JTI to the
// KV blocklist. POST /api/auth/logout, requires NewAuth + NewCSRF in front.
//
// 200 on success, 401 if the auth middleware did not populate locals
// (defensive — should be unreachable in production wiring).
func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	claims := middleware.AuthClaimsFromCtx(c)
	if claims == nil {
		return h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}

	if err := h.usecase.Logout(c.UserContext(), claims); err != nil {
		switch {
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not logout")
		}
	}

	// Clear both cookies by emitting Set-Cookie with an empty value, a
	// MaxAge of -1 (browsers translate negative MaxAge to immediate
	// eviction), and Expires in the past.
	pastExpiry := time.Unix(0, 0)
	c.Cookie(&fiber.Cookie{
		Name:     middleware.AuthCookieName,
		Value:    "",
		Path:     h.cookieAttrs.Path,
		Domain:   h.cookieAttrs.Domain,
		MaxAge:   -1,
		Expires:  pastExpiry,
		Secure:   h.cookieAttrs.Secure,
		HTTPOnly: true,
		SameSite: h.cookieAttrs.SameSite,
	})
	c.Cookie(&fiber.Cookie{
		Name:     middleware.CSRFCookieName,
		Value:    "",
		Path:     h.cookieAttrs.Path,
		Domain:   h.cookieAttrs.Domain,
		MaxAge:   -1,
		Expires:  pastExpiry,
		Secure:   h.cookieAttrs.Secure,
		HTTPOnly: false,
		SameSite: h.cookieAttrs.SameSite,
	})
	return c.JSON(statusBody{Status: "ok"})
}

// Me returns the authenticated driver. GET /api/auth/me, requires NewAuth.
//
// 200 on success, 401 if the auth middleware did not populate locals,
// 404 if the underlying driver row is gone (rare race — logout then
// account deletion).
func (h *AuthHandler) Me(c *fiber.Ctx) error {
	userID := middleware.AuthUserIDFromCtx(c)
	if userID == "" {
		return h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}

	d, err := h.usecase.Me(c.UserContext(), userID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return h.respondError(c, http.StatusNotFound, "not_found", "driver not found")
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not load driver")
		}
	}
	return c.JSON(userBody{User: toDTO(d)})
}

// ---------------------------------------------------------------------------
// Internals.
// ---------------------------------------------------------------------------

// respondError standardises the error envelope. Always returns nil because
// Fiber's middleware contract is "handler wrote response and is done";
// returning a non-nil error would surface to recover() which the
// handler-level errors are NOT meant to do.
func (h *AuthHandler) respondError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(errorBody{
		Error:     code,
		Message:   msg,
		RequestID: middleware.RequestIDFromCtx(c),
	})
}

// generateCSRFToken returns 32 bytes of crypto/rand hex-encoded. 64 hex
// chars is comfortably above the 16-byte floor recommended for CSRF
// tokens, and short enough that the resulting cookie is far below any
// browser's per-cookie size cap.
func generateCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("csrf token: rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// validationMessage flattens a validator.ValidationErrors into a single
// human-readable line. Used for the JSON error body so the SPA can
// surface a meaningful message without parsing structured errors.
func validationMessage(err error) string {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		// Non-ValidationErrors (e.g. an InvalidValidationError from a
		// nil pointer) fall back to the plain error string.
		return err.Error()
	}
	parts := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		parts = append(parts, fmt.Sprintf("%s failed %q", fe.Field(), fe.Tag()))
	}
	return strings.Join(parts, "; ")
}
