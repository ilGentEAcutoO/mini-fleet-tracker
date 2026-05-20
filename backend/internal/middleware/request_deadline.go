package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
)

// RequestDeadline returns a middleware that wraps every request's
// UserContext with a context.WithTimeout(d). Handlers and downstream
// code that pass `c.UserContext()` to outbound calls (D1 / KV / R2 /
// HTTP) inherit the deadline so a single slow dependency cannot stall
// a worker goroutine past the cap.
//
// Production wires this with d = 10s after RequestID + recover so:
//   - the recovered panic (if any) still carries the request_id,
//   - the deadline applies to handler + every downstream call but NOT to
//     the recover/logger frames.
//
// The middleware does NOT directly return 504 when the deadline fires.
// Instead it relies on the handler observing ctx.Done() and returning
// a fiber.Error of its choice. The reason: a handler that has already
// written part of a streamed response should be allowed to finish that
// flush (HTTP can't redo the status line). Pure CPU-bound handlers
// that ignore the context still run to completion — Fiber's
// ReadTimeout / WriteTimeout (15s, set in bootstrap.go) catches that
// case at the socket level.
//
// Returns a fiber.Handler so it composes with app.Use just like every
// other middleware in this package. Panics on non-positive duration
// rather than returning an error because there is no useful recovery
// at boot — a misconfigured deadline is an operator bug.
//
// TASK-062 / security review L5.
func RequestDeadline(d time.Duration) fiber.Handler {
	if d <= 0 {
		panic(fmt.Sprintf("middleware.RequestDeadline: duration must be > 0; got %v", d))
	}
	return func(c *fiber.Ctx) error {
		// context.WithTimeout derives from the existing UserContext so
		// upstream values (request-scoped logger, request_id, traces)
		// remain reachable through ctx.Value lookups. Using
		// context.Background() here would orphan all of that.
		ctx, cancel := context.WithTimeout(c.UserContext(), d)
		// Cancel always runs when c.Next() returns — releases the
		// timer goroutine the WithTimeout call started. Without this
		// every request leaks a goroutine until the deadline expires
		// (cheap but unbounded under load).
		defer cancel()

		c.SetUserContext(ctx)
		return c.Next()
	}
}
