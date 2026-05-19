package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/middleware"
	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/usecase"
)

// GeofenceUsecase is the narrow contract this handler needs from the
// usecase layer. Declared at the consumer site so tests can swap in a
// programmable stub without dragging in the concrete usecase struct.
type GeofenceUsecase interface {
	GetByVehicle(ctx context.Context, vehicleID string) (*domain.Geofence, error)
	Set(ctx context.Context, in usecase.SetGeofenceInput) (*domain.Geofence, error)
}

// GeofenceHandler is the HTTP-facing facade for the manager-only
// geofence workflow. Mirrors the structure of VehicleHandler — same
// validator, same error envelope, same denyIfNotManager pattern.
type GeofenceHandler struct {
	usecase  GeofenceUsecase
	validate *validator.Validate
}

// NewGeofenceHandler validates its inputs and returns a ready-to-route
// handler. Returns an error rather than panicking so the wiring layer
// can log a structured boot failure.
func NewGeofenceHandler(uc GeofenceUsecase) (*GeofenceHandler, error) {
	if uc == nil {
		return nil, errors.New("geofence handler: usecase is required")
	}
	return &GeofenceHandler{
		usecase:  uc,
		validate: validator.New(validator.WithRequiredStructEnabled()),
	}, nil
}

// putGeofenceRequest is the PUT /api/vehicles/:id/geofence input.
// Validator tags catch the obvious out-of-range values at the HTTP
// boundary; the usecase re-validates with the same bounds so non-HTTP
// callers (seed script, sim CLI) cannot bypass the gate.
//
// RadiusM is an int (not int64) because the SetGeofenceInput.RadiusM
// field is int. validator/v10's gte/lte work on ints just fine.
type putGeofenceRequest struct {
	CenterLat float64 `json:"center_lat" validate:"required,latitude"`
	CenterLng float64 `json:"center_lng" validate:"required,longitude"`
	RadiusM   int     `json:"radius_m"   validate:"required,gte=50,lte=50000"`
}

// geofenceDTO is the JSON projection of domain.Geofence. snake_case
// field names match the SPA's TypeScript types.
type geofenceDTO struct {
	ID        string  `json:"id"`
	VehicleID string  `json:"vehicle_id"`
	CenterLat float64 `json:"center_lat"`
	CenterLng float64 `json:"center_lng"`
	RadiusM   int     `json:"radius_m"`
	CreatedAt int64   `json:"created_at"`
}

// geofenceBody wraps a single geofence for response shaping. Mirrors
// vehicleBody / positionBody — one shape per entity, predictable for
// the SPA.
type geofenceBody struct {
	Geofence geofenceDTO `json:"geofence"`
}

// toGeofenceDTO copies a domain.Geofence into the API DTO. Nil-safe
// so the handler can call it unconditionally.
func toGeofenceDTO(g *domain.Geofence) geofenceDTO {
	if g == nil {
		return geofenceDTO{}
	}
	return geofenceDTO{
		ID:        g.ID,
		VehicleID: g.VehicleID,
		CenterLat: g.CenterLat,
		CenterLng: g.CenterLng,
		RadiusM:   g.RadiusM,
		CreatedAt: g.CreatedAt,
	}
}

// Get returns the geofence for the :id vehicle. GET
// /api/vehicles/:id/geofence — manager-only.
//
// Status codes:
//
//	200 on success (with geofenceBody)
//	401 missing auth context
//	403 not a manager
//	404 no fence set for this vehicle (or the vehicle doesn't exist)
//	500 on infra failure
//
// The 404 path collapses "no fence" and "no vehicle" into the same
// response — the SPA doesn't need to distinguish (the vehicle-detail
// page that consumes this only renders the editor below the existing
// vehicle UI, so it already knows the vehicle exists).
func (h *GeofenceHandler) Get(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	id := c.Params("id")
	g, err := h.usecase.GetByVehicle(c.UserContext(), id)
	if err != nil {
		return h.mapDomainError(c, err, "could not load geofence")
	}
	return c.JSON(geofenceBody{Geofence: toGeofenceDTO(g)})
}

// Put sets (or replaces) the geofence for the :id vehicle. PUT
// /api/vehicles/:id/geofence — manager-only.
//
// Body: { "center_lat": <float>, "center_lng": <float>, "radius_m": <int> }
//
// Status codes:
//
//	200 on success (with geofenceBody — the newly-stored fence)
//	400 on validation failure (malformed body, out-of-range values)
//	401 missing auth context
//	403 not a manager
//	404 vehicle id does not exist
//	500 on infra failure
//
// PUT (not POST) because the operation is idempotent at the resource
// level — the same body sent twice produces the same end state. The
// usecase's Upsert handles the "replace existing fence" semantic.
func (h *GeofenceHandler) Put(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	id := c.Params("id")

	var req putGeofenceRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	g, err := h.usecase.Set(c.UserContext(), usecase.SetGeofenceInput{
		VehicleID: id,
		CenterLat: req.CenterLat,
		CenterLng: req.CenterLng,
		RadiusM:   req.RadiusM,
	})
	if err != nil {
		return h.mapDomainError(c, err, "could not set geofence")
	}
	return c.JSON(geofenceBody{Geofence: toGeofenceDTO(g)})
}

// ---------------------------------------------------------------------------
// Internals — mirrors the helpers on VehicleHandler so this handler has
// the same error envelope shape and the same manager-only gate.
// ---------------------------------------------------------------------------

// denyIfNotManager writes 401 (missing auth context) or 403 (wrong
// role) and returns (true, responseErr) when the caller should stop
// processing. Returns (false, nil) when the caller is a manager.
//
// Mirrors VehicleHandler.denyIfNotManager — we duplicate rather than
// share because (a) extracting a shared helper would couple every
// handler's role gate to the same signature and (b) the duplication
// is one screen of code that has not drifted across handlers.
func (h *GeofenceHandler) denyIfNotManager(c *fiber.Ctx) (bool, error) {
	role := strings.TrimSpace(middleware.AuthRoleFromCtx(c))
	if role == "" {
		return true, h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}
	if domain.Role(role) != domain.RoleManager {
		return true, h.respondError(c, http.StatusForbidden, "forbidden", "manager role required")
	}
	return false, nil
}

// mapDomainError translates a usecase error into the standard HTTP
// envelope. The "geofence not found" mapping is deliberate (404) — a
// missing fence is a normal state, not an error condition.
func (h *GeofenceHandler) mapDomainError(c *fiber.Ctx, err error, fallback string) error {
	switch {
	case errors.Is(err, domain.ErrValidation):
		return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return h.respondError(c, http.StatusNotFound, "not_found", "geofence not found")
	case errors.Is(err, domain.ErrForbidden):
		return h.respondError(c, http.StatusForbidden, "forbidden", err.Error())
	default:
		return h.respondError(c, http.StatusInternalServerError, "internal", fallback)
	}
}

// respondError standardises the error envelope across this handler.
// Same shape every other handler emits — the SPA only learns one
// envelope structure.
func (h *GeofenceHandler) respondError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(errorBody{
		Error:     code,
		Message:   msg,
		RequestID: middleware.RequestIDFromCtx(c),
	})
}
