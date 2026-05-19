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

// PhotoUsecase is the narrow contract the photo handler needs from the
// usecase layer. Declared at the consumer site so tests can swap in a
// programmable stub without dragging in the concrete PhotoUsecase.
type PhotoUsecase interface {
	PresignUpload(ctx context.Context, userID, vehicleID, filename string) (*usecase.PresignUploadOutput, error)
	List(ctx context.Context, vehicleID string) ([]usecase.PhotoListEntry, error)
}

// PhotoHandler is the HTTP-facing facade for the Bonus 4 photo
// workflow. Both endpoints are manager-only — a driver-role caller
// receives 403.
type PhotoHandler struct {
	usecase  PhotoUsecase
	validate *validator.Validate
}

// NewPhotoHandler validates its inputs and returns a ready-to-route
// handler. Returns an error rather than panicking so the boot sequence
// can log a structured failure.
func NewPhotoHandler(uc PhotoUsecase) (*PhotoHandler, error) {
	if uc == nil {
		return nil, errors.New("photo handler: usecase is required")
	}
	return &PhotoHandler{
		usecase:  uc,
		validate: validator.New(validator.WithRequiredStructEnabled()),
	}, nil
}

// ---------------------------------------------------------------------------
// Request / response shapes.
// ---------------------------------------------------------------------------

// presignRequest is the POST :photos:presign input. Filename is the
// only field the client controls; everything else (key, content-length,
// TTL) is server-determined to keep the contract tight.
type presignRequest struct {
	Filename string `json:"filename" validate:"required,min=1,max=200"`
}

// photoListBody wraps the slice for response shaping (room to add
// pagination later without breaking the contract).
type photoListBody struct {
	VehicleID string                    `json:"vehicle_id"`
	Photos    []usecase.PhotoListEntry  `json:"photos"`
	Count     int                       `json:"count"`
}

// ---------------------------------------------------------------------------
// Handler methods.
// ---------------------------------------------------------------------------

// Presign returns a short-lived signed PUT URL for direct browser
// upload to R2. POST /api/vehicles/:id/photos:presign, manager-only.
//
// Status codes:
//
//	200 on success (with PresignUploadOutput body)
//	400 on bad body / validator failure / usecase validation error
//	401 missing auth context
//	403 not a manager
//	404 vehicle id does not exist
//	429 daily quota exhausted ({ error: "quota_exceeded" })
//	500 on infra failure
//
// Quota-exceeded surfaces as 429 with the "quota_exceeded" error code
// because the user CAN retry tomorrow — a 403 would imply a permission
// issue the user can never fix.
func (h *PhotoHandler) Presign(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}

	userID := middleware.AuthUserIDFromCtx(c)
	if userID == "" {
		// Defensive — the auth middleware should populate this. A miss
		// indicates a wiring break, surface as 401 so the SPA retries.
		return h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}

	vehicleID := c.Params("id")

	var req presignRequest
	if err := c.BodyParser(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", "invalid request body")
	}
	if err := h.validate.Struct(&req); err != nil {
		return h.respondError(c, http.StatusBadRequest, "validation_failed", validationMessage(err))
	}

	out, err := h.usecase.PresignUpload(c.UserContext(), userID, vehicleID, req.Filename)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		case errors.Is(err, domain.ErrNotFound):
			return h.respondError(c, http.StatusNotFound, "not_found", "vehicle not found")
		case errors.Is(err, domain.ErrTooMany):
			// 429 with a stable error code so the SPA can localise the
			// "try again tomorrow" message and surface a quota-specific
			// toast instead of a generic rate-limit pill.
			return c.Status(http.StatusTooManyRequests).JSON(errorBody{
				Error:     "quota_exceeded",
				Message:   "Daily upload limit reached for this vehicle. Try again tomorrow.",
				RequestID: middleware.RequestIDFromCtx(c),
			})
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not presign upload")
		}
	}

	return c.JSON(out)
}

// List returns every photo under the vehicle's prefix as {key, signed
// GET URL}. GET /api/vehicles/:id/photos, manager-only.
//
// Status codes:
//
//	200 on success (with photoListBody)
//	401 missing auth context
//	403 not a manager
//	404 vehicle id does not exist
//	500 on infra failure
func (h *PhotoHandler) List(c *fiber.Ctx) error {
	if denied, err := h.denyIfNotManager(c); denied {
		return err
	}

	vehicleID := c.Params("id")
	photos, err := h.usecase.List(c.UserContext(), vehicleID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrValidation):
			return h.respondError(c, http.StatusBadRequest, "validation_failed", err.Error())
		case errors.Is(err, domain.ErrNotFound):
			return h.respondError(c, http.StatusNotFound, "not_found", "vehicle not found")
		default:
			return h.respondError(c, http.StatusInternalServerError, "internal", "could not list photos")
		}
	}

	return c.JSON(photoListBody{
		VehicleID: vehicleID,
		Photos:    photos,
		Count:     len(photos),
	})
}

// ---------------------------------------------------------------------------
// Internals.
// ---------------------------------------------------------------------------

// denyIfNotManager mirrors the helper in vehicle_handler.go — kept
// per-handler so each one is self-contained and the role-gate rationale
// stays close to the code that uses it. If a third handler grows the
// same guard we'll promote it to a package-level helper.
func (h *PhotoHandler) denyIfNotManager(c *fiber.Ctx) (bool, error) {
	role := middleware.AuthRoleFromCtx(c)
	if role == "" {
		return true, h.respondError(c, http.StatusUnauthorized, "unauthorized", "missing auth context")
	}
	if domain.Role(role) != domain.RoleManager {
		return true, h.respondError(c, http.StatusForbidden, "forbidden", "manager role required")
	}
	return false, nil
}

// respondError matches the auth handler's envelope so the SPA only
// needs to learn one shape (errorBody from auth_handler.go).
func (h *PhotoHandler) respondError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(errorBody{
		Error:     code,
		Message:   msg,
		RequestID: middleware.RequestIDFromCtx(c),
	})
}
