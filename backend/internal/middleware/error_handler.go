package middleware

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

// JSONErrorHandler is the Fiber-level error handler that produces the
// standard JSON envelope:
//
//	{ "error": "internal", "message": "...", "request_id": "..." }
//
// It is wired via fiber.Config.ErrorHandler at app construction so EVERY
// error path — handler returning *fiber.Error, handler returning a plain
// error, and recovered panics (the recover middleware turns the panic
// value into a returned error) — emits the same shape the SPA already
// parses. Without this, recovered panics surface as Fiber's default
// text/plain "Internal Server Error" body, breaking the SPA's
// error-toast pipeline and erasing the request_id correlation that
// operators rely on for log lookup.
//
// Security choices:
//   - For panics and non-fiber errors we emit a fixed generic message
//     ("internal server error"). Surfacing the raw error string would
//     leak internals (DB row IDs, file paths, cfclient.firstErrorMessage
//     fragments). The raw error is logged server-side at error level
//     with request_id + route + IP so operators can still triage.
//   - For *fiber.Error we trust the developer-chosen Message because
//     it was set deliberately by handler code (e.g. fiber.NewError(404,
//     "vehicle not found")). The error CODE field stays "internal" to
//     keep the SPA's switch-on-error-code simple; the status carries
//     the routing intent.
//
// TASK-061 / security review L4.
func JSONErrorHandler(c *fiber.Ctx, err error) error {
	if err == nil {
		// Should never happen — Fiber only calls the ErrorHandler when
		// the handler returned non-nil. Defensive: return 500 with a
		// blank envelope so the SPA still parses something.
		return c.Status(fiber.StatusInternalServerError).
			Type(fiber.MIMEApplicationJSON).
			JSON(envelope("internal", "internal server error", RequestIDFromCtx(c)))
	}

	status := fiber.StatusInternalServerError
	message := "internal server error"

	var fe *fiber.Error
	if errors.As(err, &fe) {
		status = fe.Code
		if fe.Message != "" {
			message = fe.Message
		}
	}

	// Always log the raw error server-side. Operators correlate via
	// request_id; this is the only place that has both the panic
	// payload (translated by the recover middleware to error.Error())
	// and the request scope. Logging at Warn for *fiber.Error (those
	// are deliberate) and Error for everything else (those are bugs).
	logEvent := log.Error()
	if fe != nil {
		logEvent = log.Warn()
	}
	logEvent.
		Err(err).
		Str("request_id", RequestIDFromCtx(c)).
		Str("route", c.Path()).
		Str("method", c.Method()).
		Str("ip", c.IP()).
		Int("status", status).
		Msg("error handler: request failed")

	return c.Status(status).
		Type(fiber.MIMEApplicationJSON).
		JSON(envelope("internal", message, RequestIDFromCtx(c)))
}

// envelope is the JSON shape the rest of the API uses for error responses
// (see internal/handler/auth_handler.go:errorBody, etc). Inlined here as
// a small struct rather than imported from handler so middleware never
// has an upstream-into-handler dependency.
func envelope(code, message, requestID string) map[string]string {
	out := map[string]string{
		"error":   code,
		"message": message,
	}
	if requestID != "" {
		out["request_id"] = requestID
	}
	return out
}
