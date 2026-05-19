package domain

// Role enumerates the access tiers a Driver entity can hold. The set is
// intentionally tiny — there is no "admin" role in the demo because the
// only operations beyond a manager's reach (D1 schema migrations, KV
// namespace edits) happen out of band via wrangler.
//
// The string form is what gets persisted to D1 and what appears in JWT
// claims; the CHECK constraint in 000001_init.up.sql enforces the same
// set at the storage layer, so a drift between this enum and the DB
// would surface as an insert error rather than silent corruption.
type Role string

// Role constants. Only these two values pass Role.Valid().
const (
	RoleDriver  Role = "driver"
	RoleManager Role = "manager"
)

// Valid reports whether r is one of the recognised role strings. The
// usecase layer calls this before persisting a Driver so an invalid role
// surfaces as domain.ErrValidation rather than a database CHECK violation.
func (r Role) Valid() bool {
	switch r {
	case RoleDriver, RoleManager:
		return true
	default:
		return false
	}
}

// Driver is the authentication subject — a person who can log in to the
// fleet tracker. Field types mirror the D1 schema exactly:
//
//   - ID is a UUID v7 string generated app-side (lexicographically sortable
//     by creation time, no DB sequence required).
//   - PasswordHash is the PHC-encoded argon2id digest produced by
//     pkg/hash.HashPassword. The struct keeps the field so the repository
//     can populate it for the usecase's Login verification path, but the
//     HTTP handler layer MUST translate Driver into a DTO that omits the
//     hash before serialising — see TASK-007 for the response shape.
//   - CreatedAt / UpdatedAt are unix milliseconds for portability across
//     SQLite/D1 and time.UnixMilli.
//
// The struct is a plain data carrier: no methods, no construction-time
// invariants. The usecase layer is responsible for validation; the
// repository is responsible for persistence.
type Driver struct {
	ID           string
	Email        string
	PasswordHash string
	Name         string
	Role         Role
	CreatedAt    int64
	UpdatedAt    int64
}
