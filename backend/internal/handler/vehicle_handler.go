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

// VehicleUsecase is the narrow contract the vehicle handler needs from the
// usecase layer. Declared at the consumer site so tests can swap in a
// programmable stub without dragging in the concrete VehicleUsecase
// struct.
type VehicleUsecase interface {
	List(ctx context.Context) ([]*domain.Vehicle, error)
	Get(ctx context.Context, id string) (*domain.Vehicle, error)
	Create(ctx context.Context, in usecase.CreateVehicleInput) (*domain.Vehicle, error)
	Update(ctx context.Context, id string, in usecase.UpdateVehicleInput) (*domain.Vehicle, error)
	Delete(ctx context.Context, id string) error
	// ListPositions powers the TASK-018 history endpoint. The handler
	// converts query params into the unix-ms / limit triple; the usecase
	// owns validation and the existence check.
	ListPositions(ctx context.Context, id string, fromMs, toMs int64, limit int) ([]*domain.Position, error)
}

// VehicleHandler is the HTTP-facing facade for the vehicle CRUD workflows.
// Every method is manager-only — driver-role callers receive a 403.
type VehicleHandler struct {
	usecase  VehicleUsecase
	validate *validator.Validate
}

// NewVehicleHandler validates its inputs and returns a ready-to-route
// handler. Returns an error rather than panicking so the wiring layer can
// log a structured boot failure.
func NewVehicleHandler(uc VehicleUsecase) (*VehicleHandler, error) {
	if uc == nil {
		return nil, errors.New("vehicle handler: usecase is required")
	}
	return &VehicleHandler{
		usecase:  uc,
		validate: validator.New(validator.WithRequiredStructEnabled()),
	}, nil
}

// ---------------------------------------------------------------------------
// Request / response shapes.
// ---------------------------------------------------------------------------

// createVehicleRequest is the POST /api/vehicles input. The validator tags
// are deliberately loose — the usecase enforces tighter business rules
// (max lengths, UUID shape). The handler-level checks here are the
// minimum needed to reject obviously-malformed payloads before they reach
// the usecase.
type createVehicleRequest struct {
	PlateNumber string `json:"plate_number" validate:"required,min=1,max=50"`
	Model       string `json:"model"        validate:"omitempty,max=100"`
	DriverID    string `json:"driver_id"    validate:"omitempty,uuid4|uuid"`
}

// updateVehicleRequest is the PATCH /api/vehicles/:id input. All fields
// are pointers so the absence of a JSON key means "leave unchanged"; an
// explicit null is treated like an absent key. The usecase translates
// non-nil pointers into the underlying patch and validates lengths.
//
// The validator tags use `omitnil` so a nil pointer never trips
// validation — required fields cannot be expressed at this layer because
// the whole point of PATCH is partial input.
type updateVehicleRequest struct {
	PlateNumber *string `json:"plate_number" validate:"omitnil,min=1,max=50"`
	Model       *string `json:"model"        validate:"omitnil,max=100"`
	DriverID    *string `json:"driver_id"    validate:"omitnil"`
}

// vehicleDTO is the JSON projection of domain.Vehicle. Field names match
// the API contract (snake_case); model and driver_id are omitted when
// empty so unassigned fields do not clutter responses.
type vehicleDTO struct {
	ID          string `json:"id"`
	PlateNumber string `json:"plate_number"`
	Model       string `json:"model,omitempty"`
	DriverID    string `json:"driver_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// toVehicleDTO copies a domain.Vehicle into its public projection. Nil-
// safe to keep callers free of explicit guards.
func toVehicleDTO(v *domain.Vehicle) vehicleDTO {
	if v == nil {
		return vehicleDTO{}
	}
	return vehicleDTO{
		ID:          v.ID,
		PlateNumber: v.PlateNumber,
		Model:       v.Model,
		DriverID:    v.DriverID,
		CreatedAt:   v.CreatedAt,
		UpdatedAt:   v.UpdatedAt,
	}
}

// vehicleBody wraps a single vehicle for response shaping.
type vehicleBody struct {
	Vehicle vehicleDTO `json:"vehicle"`
}

// vehicleListBody wraps a list of vehicles for response shaping. Wrapping
// in an object (rather than returning a bare array) gives us room to add
// pagination metadata without breaking the existing contract.
type vehicleListBody struct {
	Vehicles []vehicleDTO `json:"vehicles"`
}

// historyBody is the response envelope for the GET /api/vehicles/:id/positions
// endpoint. VehicleID is repeated outside the slice so the client can
// double-check it matches the route param without parsing the first
// position; Count is supplied so the frontend can render
// "N points" without iterating the array twice.
//
// Positions reuses the same positionDTO defined in position_handler.go to
// keep one wire-shape per entity — adding a parallel "historyDTO" with
// trivially-different field names would create exactly the kind of
// drift the SPA's `Position` TypeScript interface is supposed to prevent.
type historyBody struct {
	VehicleID string        `json:"vehicle_id"`
	Positions []positionDTO `json:"positions"`
	Count     int           `json:"count"`
}

// ---------------------------------------------------------------------------
// Handler methods.
// ---------------------------------------------------------------------------

// List returns every vehicle. GET /api/vehicles, manager-only.
//
// 200 on success, 403 if the caller is not a manager, 500 on infra.
func (h *VehicleHandler) List(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	vehicles, err := h.usecase.List(c.UserContext())
	if err != nil {
		return h.respondError(c, http.StatusInternalServerError, "internal", "could not list vehicles")
	}
	dtos := make([]vehicleDTO, 0, len(vehicles))
	for _, v := range vehicles {
		dtos = append(dtos, toVehicleDTO(v))
	}
	return c.JSON(vehicleListBody{Vehicles: dtos})
}

// Get returns the vehicle with :id. GET /api/vehicles/:id, manager-only.
//
// 200 on success, 403 driver, 404 missing, 500 on infra.
func (h *VehicleHandler) Get(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	id := c.Params("id")
	v, err := h.usecase.Get(c.UserContext(), id)
	if err != nil {
		return h.mapDomainError(c, err, "could not load vehicle")
	}
	return c.JSON(vehicleBody{Vehicle: toVehicleDTO(v)})
}

// Create inserts a new vehicle. POST /api/vehicles, manager-only.
//
// 201 on success, 400 on validation, 403 driver, 409 on duplicate plate,
// 500 on infra.
func (h *VehicleHandler) Create(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	var req createVehicleRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}
	v, err := h.usecase.Create(c.UserContext(), usecase.CreateVehicleInput{
		PlateNumber: req.PlateNumber,
		Model:       req.Model,
		DriverID:    req.DriverID,
	})
	if err != nil {
		return h.mapDomainError(c, err, "could not create vehicle")
	}
	return c.Status(http.StatusCreated).JSON(vehicleBody{Vehicle: toVehicleDTO(v)})
}

// Update patches the vehicle with :id. PATCH /api/vehicles/:id, manager-only.
//
// 200 on success, 400 on validation, 403 driver, 404 missing, 409 on
// plate collision, 500 on infra.
func (h *VehicleHandler) Update(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	id := c.Params("id")

	var req updateVehicleRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	v, err := h.usecase.Update(c.UserContext(), id, usecase.UpdateVehicleInput{
		PlateNumber: req.PlateNumber,
		Model:       req.Model,
		DriverID:    req.DriverID,
	})
	if err != nil {
		return h.mapDomainError(c, err, "could not update vehicle")
	}
	return c.JSON(vehicleBody{Vehicle: toVehicleDTO(v)})
}

// Delete removes the vehicle with :id. DELETE /api/vehicles/:id, manager-only.
//
// 204 on success, 403 driver, 404 missing, 500 on infra. The 204
// (No Content) is chosen over 200 because the response body would
// otherwise be empty — explicit semantics are friendlier to clients.
func (h *VehicleHandler) Delete(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}
	id := c.Params("id")
	if err := h.usecase.Delete(c.UserContext(), id); err != nil {
		return h.mapDomainError(c, err, "could not delete vehicle")
	}
	return c.SendStatus(http.StatusNoContent)
}

// History returns up to `limit` positions for the :id vehicle in the
// optional range [from, to] (both unix-ms; either or both may be omitted
// or zero to mean "no bound"). GET /api/vehicles/:id/positions, manager-only.
//
// Query parameters:
//
//	from   optional unix-ms, default 0 = "no lower bound"
//	to     optional unix-ms, default 0 = "no upper bound"
//	limit  optional int, default 1000, hard-capped at 5000
//
// The handler parses & forwards; the usecase owns validation (range
// ordering, non-negativity, limit clamping) and the existence check.
//
// Status codes:
//
//	200 on success (with historyBody)
//	400 on validation (from > to, negative bounds, etc.)
//	401 missing auth context
//	403 not a manager
//	404 vehicle id does not exist
//	500 on infra failure
//
// The DESC ordering by recorded_at comes from the repository contract.
// Clients that need chronological order for polyline rendering should
// reverse the slice on receive — see the frontend history page.
func (h *VehicleHandler) History(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}

	id := c.Params("id")

	// QueryInt returns 0 when the key is absent or unparseable, which is
	// exactly the "no bound" sentinel the usecase expects. The handler
	// does not need its own zero-check — the usecase's validation rejects
	// the only ambiguous case (a literally-negative integer).
	fromMs := int64(c.QueryInt("from", 0))
	toMs := int64(c.QueryInt("to", 0))
	limit := c.QueryInt("limit", 0)

	positions, err := h.usecase.ListPositions(c.UserContext(), id, fromMs, toMs, limit)
	if err != nil {
		return h.mapDomainError(c, err, "could not list positions")
	}

	dtos := make([]positionDTO, 0, len(positions))
	for _, p := range positions {
		dtos = append(dtos, toPositionDTO(p))
	}
	return c.JSON(historyBody{
		VehicleID: id,
		Positions: dtos,
		Count:     len(dtos),
	})
}

// ---------------------------------------------------------------------------
// Internals.
// ---------------------------------------------------------------------------

// denyIfNotManager writes a 401 (missing auth context) or 403 (wrong role)
// response and returns (responseErr, true) when the caller should stop
// processing. Returns (nil, false) when the caller is a manager and the
// handler should continue.
//
// The middleware order in production wiring is: auth -> csrf (for
// mutators) -> handler. By the time we reach this guard the caller must
// have a valid token; an empty role string indicates either a
// misconfigured pipeline or — far more likely in a test — a request
// that bypassed the auth middleware entirely. Treating that as 401 is
// the safest default.
//
// The two-value signature (denied bool, err error) is deliberate: Fiber's
// c.Status(...).JSON(...) returns nil on a successful write, so the
// caller cannot use a non-nil error alone to mean "response sent". The
// boolean carries that signal unambiguously while the error keeps any
// downstream JSON-write failure reportable.
//
// Go convention puts the error last; callers read as:
//
//	if denied, err := h.denyIfNotManager(c); denied { return err }
func (h *VehicleHandler) denyIfNotManager(c *fiber.Ctx) (bool, error) {
	role := strings.TrimSpace(middleware.AuthRoleFromCtx(c))
	if role == "" {
		return true, h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}
	if domain.Role(role) != domain.RoleManager {
		return true, h.respondError(c, http.StatusForbidden, "forbidden", "manager role required")
	}
	return false, nil
}

// mapDomainError translates a usecase/repo error into the standard HTTP
// envelope. fallback is the message used for the catch-all 500 path so
// each handler method can supply context-specific copy ("could not list
// vehicles" vs "could not update vehicle") without re-implementing the
// switch.
func (h *VehicleHandler) mapDomainError(c *fiber.Ctx, err error, fallback string) error {
	switch {
	case errors.Is(err, domain.ErrValidation):
		return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return h.respondError(c, http.StatusNotFound, "not_found", "vehicle not found")
	case errors.Is(err, domain.ErrAlreadyExists):
		return h.respondError(c, http.StatusConflict, "already_exists", "plate number already in use")
	case errors.Is(err, domain.ErrForbidden):
		return h.respondError(c, http.StatusForbidden, "forbidden", err.Error())
	default:
		return h.respondError(c, http.StatusInternalServerError, "internal", fallback)
	}
}

// respondError standardises the error envelope. Mirrors AuthHandler's
// helper so the SPA only learns one envelope shape across endpoints.
func (h *VehicleHandler) respondError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(errorBody{
		Error:     code,
		Message:   msg,
		RequestID: middleware.RequestIDFromCtx(c),
	})
}
