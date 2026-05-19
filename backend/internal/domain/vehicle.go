package domain

// Vehicle is a fleet asset whose position is tracked over time. Field types
// mirror the D1 schema exactly:
//
//   - ID is a UUID v7 string generated app-side (lexicographically sortable
//     by creation time, no DB sequence required).
//   - PlateNumber is the human-facing licence-plate identifier and is
//     UNIQUE in the D1 schema; duplicates surface as domain.ErrAlreadyExists.
//   - Model is the make/model description ("Toyota Hilux"); the column is
//     nullable. An empty string represents "unset" — the repository layer
//     maps that to SQL NULL on write and back to "" on read.
//   - DriverID is a foreign key to drivers.id with ON DELETE SET NULL. An
//     empty string represents "unassigned" and follows the same null
//     translation rule as Model.
//   - CreatedAt / UpdatedAt are unix milliseconds for portability across
//     SQLite/D1 and Go's time.UnixMilli.
//
// The struct is a plain data carrier: no methods, no construction-time
// invariants. The usecase layer is responsible for validation; the
// repository is responsible for persistence.
//
// Convention for nullable string fields: this package uses empty string
// rather than *string for Model and DriverID. The tradeoff is that we
// cannot distinguish "explicitly set to empty" from "unset", but that
// distinction has no meaning for these two fields in the fleet domain:
// a plate number cannot be "the empty string" and an unassigned driver
// is indistinguishable from "no driver".
type Vehicle struct {
	ID          string
	PlateNumber string
	Model       string
	DriverID    string
	CreatedAt   int64
	UpdatedAt   int64
}
