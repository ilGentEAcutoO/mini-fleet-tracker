package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
)

// PositionUsecase is the narrow contract this handler needs from the
// usecase layer. Declared at the consumer site so tests can mock the
// surface without importing the concrete usecase struct.
type PositionUsecase interface {
	Write(ctx context.Context, driverID string, in usecase.WritePositionInput) (*domain.Position, error)
}

// PositionHandler is the HTTP-facing facade for the driver-only position
// write workflow. Construction is parallel to AuthHandler — small struct,
// dependencies passed in, single endpoint registered by the wiring layer.
type PositionHandler struct {
	usecase  PositionUsecase
	validate *validator.Validate
}

// NewPositionHandler validates its inputs and returns a ready-to-route
// handler. Returns an error rather than panicking so the boot sequence
// can log a structured failure.
func NewPositionHandler(uc PositionUsecase) (*PositionHandler, error) {
	if uc == nil {
		return nil, errors.New("position handler: usecase is required")
	}
	return &PositionHandler{
		usecase:  uc,
		validate: validator.New(validator.WithRequiredStructEnabled()),
	}, nil
}

// writePositionRequest is the POST /api/positions input.
//
// Validation tags use validator/v10's built-in latitude/longitude
// validators as the first line of defense (they enforce the same
// numeric bounds the usecase re-checks). The usecase validates again
// because the seed script and the sim CLI can invoke the usecase
// without crossing the HTTP boundary.
//
// SpeedKmh is optional — `omitempty` on the JSON tag elides it on the
// response side, and the validator's `gte=0,lte=500` only runs when the
// caller supplies the field. validator/v10 treats the Go zero value as
// "missing" for non-required fields, so omitting the JSON key in the
// request body bypasses the speed check entirely (matching the
// "unset = zero" domain convention).
type writePositionRequest struct {
	VehicleID  string  `json:"vehicle_id"           validate:"required"`
	Lat        float64 `json:"lat"                  validate:"required,latitude"`
	Lng        float64 `json:"lng"                  validate:"required,longitude"`
	SpeedKmh   float64 `json:"speed_kmh,omitempty"  validate:"omitempty,gte=0,lte=500"`
	RecordedAt int64   `json:"recorded_at"          validate:"required"`
}

// positionDTO is the public projection of domain.Position. We expose
// every field except the internal book-keeping (none today), with
// SpeedKmh omitempty so a "0" doesn't show up in responses where the
// driver did not report a speed.
type positionDTO struct {
	ID         int64   `json:"id"`
	VehicleID  string  `json:"vehicle_id"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	SpeedKmh   float64 `json:"speed_kmh,omitempty"`
	RecordedAt int64   `json:"recorded_at"`
	CreatedAt  int64   `json:"created_at"`
}

// positionBody is the standard 2xx envelope when a single position is
// returned. Mirrors the userBody pattern from auth_handler.go so the SPA
// learns one shape per entity.
type positionBody struct {
	Position positionDTO `json:"position"`
}

// toPositionDTO copies a domain.Position into the API DTO. Nil-safe so
// the handler can call it unconditionally.
func toPositionDTO(p *domain.Position) positionDTO {
	if p == nil {
		return positionDTO{}
	}
	return positionDTO{
		ID:         p.ID,
		VehicleID:  p.VehicleID,
		Lat:        p.Lat,
		Lng:        p.Lng,
		SpeedKmh:   p.SpeedKmh,
		RecordedAt: p.RecordedAt,
		CreatedAt:  p.CreatedAt,
	}
}

// Write is the POST /api/positions handler. Driver-only: a caller whose
// JWT role claim is not "driver" gets a 403 before the body is parsed.
// The wiring layer composes NewAuth + NewCSRF + this handler in that
// order; the role check is in the handler (not a separate middleware)
// because the role gate is a single-line conditional and a dedicated
// middleware would add wiring complexity that isn't justified for one
// endpoint.
//
// Status codes:
//
//	201 on success (with positionDTO body)
//	400 on bad body / validator failure / usecase validation error
//	401 on missing or invalid auth cookie (handled by NewAuth upstream)
//	403 on wrong role OR when the authenticated driver does not own the
//	    target vehicle (the latter via ErrForbidden from the usecase)
//	404 when the target vehicle does not exist
//	500 on infrastructure failure
func (h *PositionHandler) Write(c *fiber.Ctx) error {
	driverID := middleware.AuthUserIDFromCtx(c)
	if driverID == "" {
		// Defensive — NewAuth should have populated locals before we
		// got here. A miss means the wiring is broken; return 401 so
		// the SPA retries rather than a confusing 500.
		return h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}

	// Role gate. Only drivers may submit positions; a manager attempting
	// this is almost certainly a misconfigured client, not a malicious
	// caller, but the response is the same 403 envelope for safety. We
	// check this before BodyParser so a bad-role request doesn't even
	// pay the JSON parse cost.
	if role := middleware.AuthRoleFromCtx(c); role != string(domain.RoleDriver) {
		return h.respondError(c, http.StatusForbidden, "forbidden", "only drivers can submit positions")
	}

	var req writePositionRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	p, err := h.usecase.Write(c.UserContext(), driverID, usecase.WritePositionInput{
		VehicleID:  req.VehicleID,
		Lat:        req.Lat,
		Lng:        req.Lng,
		SpeedKmh:   req.SpeedKmh,
		RecordedAt: req.RecordedAt,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		case errors.Is(err, domain.ErrNotFound):
			return h.respondError(c, http.StatusNotFound, "not_found", "vehicle not found")
		case errors.Is(err, domain.ErrForbidden):
			return h.respondError(c, http.StatusForbidden, "forbidden", "driver does not own this vehicle")
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not write position")
		}
	}

	return c.Status(http.StatusCreated).JSON(positionBody{Position: toPositionDTO(p)})
}

// respondError matches the auth handler's envelope so the SPA only
// needs to learn one shape (errorBody from auth_handler.go).
func (h *PositionHandler) respondError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(errorBody{
		Error:     code,
		Message:   msg,
		RequestID: middleware.RequestIDFromCtx(c),
	})
}
