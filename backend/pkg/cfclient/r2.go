package cfclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2Client wraps an aws-sdk-go-v2 S3 client and its presigner, configured
// to talk to Cloudflare R2's S3-compatible endpoint.
//
// Why aws-sdk-go-v2 rather than hand-rolled SigV4: R2 advertises full
// S3-compat for SigV4 query-string signed URLs, and the SDK's presigner
// is well-tested and 200+ lines smaller than the equivalent crypto code
// we'd own. The tradeoff is a heftier dependency tree (~12 modules) — an
// acceptable cost for a service we'll touch in TASK-022 (photo upload).
type R2Client struct {
	bucket    string
	s3Client  *s3.Client
	presigner *s3.PresignClient
}

// R2Config is the constructor input for NewR2Client.
type R2Config struct {
	// Endpoint is the R2 account host, e.g. https://<accountid>.r2.cloudflarestorage.com.
	// Tests can point this at an httptest.Server to inspect the signed URL.
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	// Region defaults to "auto" — R2 ignores region but the SDK still
	// signs with it, so the value must be stable between client and server.
	Region     string
	BucketName string
}

// NewR2Client validates cfg and builds an S3 client + presigner. The
// caller's context is only used for the SDK's config load (which is
// in-memory, so cancellation is a no-op here today but kept for forward-
// compat with future SDK versions that may do remote credential dance).
//
// Endpoint configuration: we set BaseEndpoint on s3.Options rather than
// implementing a custom EndpointResolverV2. The BaseEndpoint approach is
// the SDK-recommended way to point at an S3-compatible host (per the
// service/s3 docs) and it preserves the SDK's bucket-and-key composition,
// path-style toggling, and signing logic without us having to mirror them.
// A custom EndpointResolverV2 would let us assemble the URI ourselves but
// would also force us to re-implement bucket-into-path logic for
// ForcePathStyle, which is exactly the kind of detail R2's S3-compat layer
// wants the SDK to handle for us.
func NewR2Client(ctx context.Context, cfg R2Config) (*R2Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("cfclient.R2: Endpoint is required")
	}
	if strings.TrimSpace(cfg.AccessKeyID) == "" {
		return nil, errors.New("cfclient.R2: AccessKeyID is required")
	}
	if strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return nil, errors.New("cfclient.R2: SecretAccessKey is required")
	}
	if strings.TrimSpace(cfg.BucketName) == "" {
		return nil, errors.New("cfclient.R2: BucketName is required")
	}

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "auto"
	}

	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("cfclient.R2: parse endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("cfclient.R2: endpoint %q missing scheme or host", cfg.Endpoint)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("cfclient.R2: load aws config: %w", err)
	}

	endpointStr := cfg.Endpoint
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(endpointStr)
		// Region must match what the SDK uses to sign; redundant with
		// awsconfig.WithRegion above but explicit here for clarity.
		o.Region = region
	})

	return &R2Client{
		bucket:    cfg.BucketName,
		s3Client:  s3Client,
		presigner: s3.NewPresignClient(s3Client),
	}, nil
}

// PresignPutObject returns a presigned URL for a PUT upload of `key`. The
// URL is valid for ttl. contentLengthMax is an advisory upper bound on the
// uploaded object size:
//
//   - When > 0, the SDK includes `Content-Length` in the signed headers
//     and the SignedHeaders entry — meaning the client MUST send exactly
//     that length, and any deviation fails the SigV4 check at R2's edge.
//   - When == 0, no length is signed and the client may upload any size
//     within R2's account/plan limits.
//
// A true content-length *range* (min ≤ N ≤ max) is only expressible via
// POST policy uploads, which the S3 SDK presigner does not generate. For
// TASK-005 the exact-length approach is adequate; TASK-022 (photo upload
// with quota) will revisit whether we need a policy-based PUT instead.
//
// The returned http.Header carries any signed headers the client must
// echo on the actual upload — at minimum `Host`, and (when capped)
// `Content-Length`. Callers should forward all of these verbatim.
func (c *R2Client) PresignPutObject(ctx context.Context, key string, ttl time.Duration, contentLengthMax int64) (*url.URL, http.Header, error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil, errors.New("cfclient.R2.PresignPutObject: key is required")
	}
	if ttl <= 0 {
		return nil, nil, errors.New("cfclient.R2.PresignPutObject: ttl must be > 0")
	}

	in := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if contentLengthMax > 0 {
		in.ContentLength = aws.Int64(contentLengthMax)
	}

	presigned, err := c.presigner.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return nil, nil, fmt.Errorf("cfclient.R2.PresignPutObject: %w", err)
	}

	u, err := url.Parse(presigned.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("cfclient.R2.PresignPutObject: parse URL: %w", err)
	}
	// presigned.SignedHeader is a textproto.MIMEHeader; copy to http.Header
	// so callers don't need to import the smithy types.
	hdr := make(http.Header, len(presigned.SignedHeader))
	for k, vs := range presigned.SignedHeader {
		hdr[k] = append(hdr[k], vs...)
	}
	return u, hdr, nil
}

// PresignGetObject returns a presigned URL for a GET of `key`, valid for ttl.
// GET presigning has no length concerns; the caller just opens the URL.
func (c *R2Client) PresignGetObject(ctx context.Context, key string, ttl time.Duration) (*url.URL, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("cfclient.R2.PresignGetObject: key is required")
	}
	if ttl <= 0 {
		return nil, errors.New("cfclient.R2.PresignGetObject: ttl must be > 0")
	}

	presigned, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return nil, fmt.Errorf("cfclient.R2.PresignGetObject: %w", err)
	}
	u, err := url.Parse(presigned.URL)
	if err != nil {
		return nil, fmt.Errorf("cfclient.R2.PresignGetObject: parse URL: %w", err)
	}
	return u, nil
}
