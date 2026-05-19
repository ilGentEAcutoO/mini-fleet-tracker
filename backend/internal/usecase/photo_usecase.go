package usecase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ilGentEAcutoO/mini-fleet-tracker/backend/internal/domain"
)

// quotaStore is the narrow KV contract PhotoUsecase needs for the
// per-vehicle daily quota. Declared at the consumer site so the usecase
// owns its dependency; the production *cfclient.KVClient satisfies this
// directly (the same way Blocklist does for AuthUsecase).
//
// Get returns (nil, false, nil) when the key is absent. Put with a
// non-zero ttl asks CF KV to expire the value after roughly that
// duration (subject to the 60s floor cfclient.KVClient enforces).
type quotaStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// photoPresigner is the narrow R2 contract PhotoUsecase needs. The
// production *cfclient.R2Client satisfies it via PresignPutObject /
// PresignGetObject / ListObjects.
//
// ListObjects returns the object keys under the given prefix; see
// pkg/cfclient/r2.go for the contract (1000-key cap, no pagination).
type photoPresigner interface {
	PresignPutObject(ctx context.Context, key string, ttl time.Duration, contentLengthMax int64) (*url.URL, http.Header, error)
	PresignGetObject(ctx context.Context, key string, ttl time.Duration) (*url.URL, error)
	ListObjects(ctx context.Context, prefix string) ([]string, error)
}

// photoVehicleLookup is the narrow contract PhotoUsecase needs to
// verify a vehicle exists before presigning an upload. The production
// d1.VehicleRepo and the existing usecase.vehicleLookup interface both
// satisfy this; we declare a separate name (rather than reusing
// geofence_usecase.go's vehicleExistenceLookup) so each usecase keeps
// ownership of its dependency surface — Go's structural typing means
// any conforming repository plugs in without coupling the usecases.
type photoVehicleLookup interface {
	Get(ctx context.Context, id string) (*domain.Vehicle, error)
}

// PhotoUsecase wires the Bonus 4 photo-upload workflow:
//
//   - PresignUpload checks the per-vehicle daily quota and returns a
//     short-lived presigned PUT URL for direct browser upload to R2.
//   - List returns presigned GET URLs for every photo under the
//     vehicle's prefix so the SPA can render thumbnails.
//
// Dependencies are immutable after construction. Safe for concurrent
// use as long as the injected adapters are.
type PhotoUsecase struct {
	presigner photoPresigner
	quotas    quotaStore
	vehicles  photoVehicleLookup
	ids       IDGenerator
	// now is the testable clock. Production leaves it nil so time.Now
	// is used; tests pin it to a fixed instant so quota date arithmetic
	// is deterministic.
	now func() time.Time
}

// Defaults and bounds — exported so handlers (and tests) reference the
// same single source of truth.
const (
	// DailyPhotoQuotaPerVehicle is the maximum number of upload
	// presigns per vehicle per UTC day. Three is a deliberately low
	// number for a demo: it makes the quota observable without
	// generating real R2 bills.
	DailyPhotoQuotaPerVehicle = 3

	// PresignPutTTL is how long an upload URL is valid. Five minutes is
	// generous for a mobile camera capture + network upload and short
	// enough that a leaked URL cannot be reused indefinitely.
	PresignPutTTL = 5 * time.Minute

	// PresignGetTTL is how long a thumbnail URL is valid. One hour
	// covers a typical dashboard session without forcing a refresh and
	// keeps R2 list-and-presign traffic to once-per-session.
	PresignGetTTL = 1 * time.Hour

	// MaxPhotoBytes is the per-upload content-length cap. 5 MB
	// accommodates a smartphone JPEG without inviting abuse.
	MaxPhotoBytes int64 = 5 * 1024 * 1024

	// quotaTTL — the per-day key needs to outlive the calendar day so
	// the final increment that happens at 23:59 isn't immediately
	// erased by the KV minimum. 26h gives 2h slack across DST
	// transitions and clock skew while still letting the key roll over
	// before the next day's first upload.
	quotaTTL = 26 * time.Hour

	// maxFilenameLen — clients pass an original filename for the
	// object key suffix. 100 is comfortable for human-readable names
	// and avoids R2 / S3 key-length anxieties (1024 byte hard cap).
	maxFilenameLen = 100

	// vehiclePhotoPrefix is the R2 key prefix template per vehicle.
	// Pull keys lazily via vehiclePrefix(id) so the format lives in
	// one place.
	vehiclePhotoPrefixFmt = "vehicles/%s/"
)

// NewPhotoUsecase constructs a usecase from its dependencies. Every
// argument is required; passing any nil returns an error rather than
// panicking on the first request.
func NewPhotoUsecase(
	presigner photoPresigner,
	quotas quotaStore,
	vehicles photoVehicleLookup,
	ids IDGenerator,
) (*PhotoUsecase, error) {
	if presigner == nil {
		return nil, errors.New("photo usecase: presigner is required")
	}
	if quotas == nil {
		return nil, errors.New("photo usecase: quotas store is required")
	}
	if vehicles == nil {
		return nil, errors.New("photo usecase: vehicles lookup is required")
	}
	if ids == nil {
		return nil, errors.New("photo usecase: id generator is required")
	}
	return &PhotoUsecase{
		presigner: presigner,
		quotas:    quotas,
		vehicles:  vehicles,
		ids:       ids,
	}, nil
}

// nowFunc returns the test-overridable clock or time.Now in production.
func (u *PhotoUsecase) nowFunc() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// PresignUploadOutput is the JSON-friendly response returned to the
// presign endpoint. The frontend uses the URL+Headers+Method to issue a
// direct PUT to R2; ContentLengthMax is echoed so the client can reject
// over-sized files before paying the upload cost.
//
// QuotaRemaining is the number of additional presigns the caller can
// request today AFTER this one; a value of 0 means "this was the last
// upload for today".
type PresignUploadOutput struct {
	URL              string            `json:"url"`
	Method           string            `json:"method"`
	Headers          map[string]string `json:"headers"`
	Key              string            `json:"key"`
	ContentLengthMax int64             `json:"content_length_max"`
	ExpiresAt        int64             `json:"expires_at"`
	QuotaRemaining   int               `json:"quota_remaining"`
}

// PhotoListEntry is one row in the List response: the R2 object key
// (stable identifier) plus a short-lived signed GET URL that the
// browser can dereference directly.
type PhotoListEntry struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

// PresignUpload checks the per-vehicle daily quota, generates a fresh
// object key, and returns the signed PUT URL the client must use to
// upload the bytes directly to R2.
//
// Validation order — cheapest first:
//  1. vehicleID non-empty + vehicle exists (one D1 Get).
//  2. filename non-empty, ≤ 100 chars, sanitised to alphanumeric +
//     `.-_` (so we never produce a key that needs URL-encoding).
//  3. KV-backed quota: count under quota:{vehicleID}:{YYYY-MM-DD};
//     if ≥ DailyPhotoQuotaPerVehicle return ErrTooMany.
//
// On success we increment the quota counter BEFORE returning the URL.
// This is a deliberate "fail closed" choice — the counter is best-effort
// (read + parse + Put without CAS), so concurrent presigns could
// overshoot the quota by a small N in the worst case. For a 3/day
// quota on a single-driver vehicle the practical overshoot is zero;
// for a manager-driven dashboard with two open tabs we may grant a
// 4th presign once per day. The alternative (defer the increment until
// after R2 confirms the upload) requires a webhook from R2 that we do
// not have in TASK-022, and would defeat the whole "presigned URL"
// flow by re-introducing a server-side checkpoint.
//
// Object key format: vehicles/{vehicleID}/{newID}-{safe_filename}.
// The newID prefix prevents collisions when two managers race uploads
// of the same filename; the suffix keeps the original name visible in
// the bucket listing for human debuggability.
func (u *PhotoUsecase) PresignUpload(
	ctx context.Context,
	userID, vehicleID, filename string,
) (*PresignUploadOutput, error) {
	if u == nil {
		return nil, errors.New("photo usecase: nil receiver")
	}

	userID = strings.TrimSpace(userID)
	vehicleID = strings.TrimSpace(vehicleID)
	if vehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required: %w", domain.ErrValidation)
	}
	// userID is informational — it scopes nothing today (the quota is
	// per-vehicle) but we keep it on the call surface so a future
	// "per-user-per-day" quota dimension can land without breaking the
	// handler contract. A blank userID is rejected so a misconfigured
	// auth pipeline surfaces here, not as a confusing missing-quota
	// row.
	if userID == "" {
		return nil, fmt.Errorf("user_id is required: %w", domain.ErrValidation)
	}

	safe, err := sanitiseFilename(filename)
	if err != nil {
		return nil, err
	}

	// Vehicle existence — propagates ErrNotFound verbatim so the
	// handler maps to 404. We do this BEFORE the quota check so a 404
	// doesn't burn a KV round-trip.
	if _, lookupErr := u.vehicles.Get(ctx, vehicleID); lookupErr != nil {
		return nil, lookupErr
	}

	now := u.nowFunc()
	quotaKey := quotaKeyFor(vehicleID, now)

	used, err := u.readQuotaCount(ctx, quotaKey)
	if err != nil {
		return nil, fmt.Errorf("photo presign: quota read: %w", err)
	}
	if used >= DailyPhotoQuotaPerVehicle {
		// 429 at the handler; the SPA surfaces "try again tomorrow".
		return nil, fmt.Errorf("daily upload limit (%d) reached for vehicle %s: %w",
			DailyPhotoQuotaPerVehicle, vehicleID, domain.ErrTooMany)
	}

	// Build the key. We pull a fresh ID from the generator — production
	// wiring uses uuid.NewString (v4); tests inject a deterministic
	// counter so assertions are stable. UUIDv7 would also be acceptable
	// (its time prefix is nice for human debugging of ListObjects
	// output) but is not required for correctness; choosing v4 keeps
	// us aligned with the rest of the codebase.
	objectKey := path.Join(fmt.Sprintf(vehiclePhotoPrefixFmt, vehicleID), u.ids.NewID()+"-"+safe)

	signedURL, signedHdr, err := u.presigner.PresignPutObject(ctx, objectKey, PresignPutTTL, MaxPhotoBytes)
	if err != nil {
		return nil, fmt.Errorf("photo presign: PresignPutObject: %w", err)
	}

	// Increment the counter. Best-effort: a Put failure is logged but
	// does not fail the request, because the URL we just minted is
	// already valid for 5 minutes and the operator would rather see
	// the upload land than have the user retry while a transient KV
	// hiccup heals. The cost of the rare "we under-counted by one"
	// case is one extra upload past the cap — acceptable for a demo,
	// and observable in logs.
	if putErr := u.writeQuotaCount(ctx, quotaKey, used+1); putErr != nil {
		log.Warn().
			Err(putErr).
			Str("vehicle_id", vehicleID).
			Str("user_id", userID).
			Str("quota_key", quotaKey).
			Int("used_before", used).
			Msg("photo presign: quota increment failed; URL issued anyway")
	}

	// Convert SDK's signed-header map to a flat map[string]string for
	// the JSON response. http.Header is []string per key; we only emit
	// the first value because SigV4 never produces multi-valued
	// signed headers (Content-Length is a single number).
	hdr := make(map[string]string, len(signedHdr))
	for k, vs := range signedHdr {
		if len(vs) > 0 {
			hdr[k] = vs[0]
		}
	}

	return &PresignUploadOutput{
		URL:              signedURL.String(),
		Method:           http.MethodPut,
		Headers:          hdr,
		Key:              objectKey,
		ContentLengthMax: MaxPhotoBytes,
		ExpiresAt:        now.Add(PresignPutTTL).UnixMilli(),
		QuotaRemaining:   DailyPhotoQuotaPerVehicle - (used + 1),
	}, nil
}

// List returns every photo under the vehicle's prefix as a slice of
// {key, signed GET URL}. A vehicle with no uploads returns an empty
// (non-nil) slice so the caller can iterate without a nil guard.
//
// The signed URLs are minted with PresignGetTTL (1h). The handler does
// NOT batch — each ListObjects result triggers one PresignGetObject
// per key. At 3 keys per vehicle this is fine; a future "many photos"
// world would want to cache the signed URLs in KV with a matching
// TTL.
func (u *PhotoUsecase) List(ctx context.Context, vehicleID string) ([]PhotoListEntry, error) {
	if u == nil {
		return nil, errors.New("photo usecase: nil receiver")
	}

	vehicleID = strings.TrimSpace(vehicleID)
	if vehicleID == "" {
		return nil, fmt.Errorf("vehicle_id is required: %w", domain.ErrValidation)
	}

	// Existence probe — same rationale as PresignUpload: a 404 for an
	// unknown vehicle is unambiguous and avoids burning the R2 list call.
	if _, err := u.vehicles.Get(ctx, vehicleID); err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf(vehiclePhotoPrefixFmt, vehicleID)
	keys, err := u.presigner.ListObjects(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("photo list: list objects: %w", err)
	}

	expiresAt := u.nowFunc().Add(PresignGetTTL).UnixMilli()
	out := make([]PhotoListEntry, 0, len(keys))
	for _, k := range keys {
		signedURL, presignErr := u.presigner.PresignGetObject(ctx, k, PresignGetTTL)
		if presignErr != nil {
			// One bad key shouldn't tank the whole list — log and skip.
			// The next refresh has a fresh chance, and the SPA's
			// thumbnail grid degrades gracefully on a missing entry.
			log.Warn().
				Err(presignErr).
				Str("vehicle_id", vehicleID).
				Str("object_key", k).
				Msg("photo list: presign GET failed; skipping entry")
			continue
		}
		out = append(out, PhotoListEntry{
			Key:       k,
			URL:       signedURL.String(),
			ExpiresAt: expiresAt,
		})
	}
	return out, nil
}

// readQuotaCount fetches the current per-day count from KV. A missing
// key reads as zero — the first upload of the day creates the row.
func (u *PhotoUsecase) readQuotaCount(ctx context.Context, key string) (int, error) {
	raw, found, err := u.quotas.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	n, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if parseErr != nil {
		// A malformed counter is operator data corruption — refuse to
		// keep counting rather than silently reset, otherwise a single
		// poison value would let an attacker reset their quota by
		// inserting garbage.
		return 0, fmt.Errorf("parse quota counter %q: %w", string(raw), parseErr)
	}
	return n, nil
}

// writeQuotaCount writes n back under the same key with the 26h TTL.
// CF KV's 60-second minimum is far below 26h so the rounding-up in
// cfclient.KVClient does not affect us.
func (u *PhotoUsecase) writeQuotaCount(ctx context.Context, key string, n int) error {
	return u.quotas.Put(ctx, key, []byte(strconv.Itoa(n)), quotaTTL)
}

// quotaKeyFor returns the per-vehicle-per-day KV key. The "quota:" prefix
// matches the convention documented in plan.md's KV table; the YYYY-MM-DD
// suffix is UTC so the day boundary is unambiguous across timezones.
//
// Note: we key by vehicleID, not userID. This matches the task spec
// ("3 uploads per vehicle per day") and is the right granularity for
// preventing R2 abuse — a manager cannot share-then-circumvent the
// limit by handing off to another driver. A future "per-user" variant
// (e.g. for audit-trail rate limits) would add a second key dimension.
func quotaKeyFor(vehicleID string, t time.Time) string {
	return fmt.Sprintf("quota:%s:%s", vehicleID, t.UTC().Format("2006-01-02"))
}

// sanitiseFilename strips path separators and replaces any character
// that isn't alphanumeric / `.-_` with `_`. Trims trailing/leading
// dots so an attacker cannot synthesise dot-prefixed object keys
// (R2 doesn't care about hidden files but the consistency is nice).
//
// Returns ErrValidation on empty input or an over-long name. The
// returned string is safe to embed in an R2 object key with no
// URL-encoding required.
func sanitiseFilename(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", fmt.Errorf("filename is required: %w", domain.ErrValidation)
	}
	if len(in) > maxFilenameLen {
		return "", fmt.Errorf("filename too long (max %d): %w", maxFilenameLen, domain.ErrValidation)
	}

	// path.Base drops anything that looks like a directory traversal.
	// We follow up with a character-by-character sweep because Base
	// allows characters R2 keys can contain but URLs cannot encode
	// without escaping (spaces, parens, etc.).
	base := path.Base(in)
	// Defence in depth — path.Base of "/" or "." returns "." or "/",
	// which we then sanitise into something obviously fake.
	if base == "." || base == "/" || base == "\\" {
		return "", fmt.Errorf("filename %q invalid: %w", in, domain.ErrValidation)
	}

	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "", fmt.Errorf("filename %q sanitised to empty: %w", in, domain.ErrValidation)
	}
	return out, nil
}
