package media

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/dezmymachine/farmish-backend/internal/config"
)

// R2 is the real Storage: Cloudflare R2 through its S3-compatible API.
// R2 has region "auto" and no ACLs; public reads come from the bucket's
// public domain.
type R2 struct {
	client        *s3.Client
	presign       *s3.PresignClient
	bucket        string
	publicBaseURL string
}

var _ Storage = (*R2)(nil)

// NewR2 builds a client for cfg. An empty Endpoint means real R2, derived
// from the account id; a set Endpoint is a local S3-compatible stand-in.
func NewR2(cfg config.R2) (*R2, error) {
	if !cfg.Configured() {
		return nil, errors.New("media storage is not configured (R2_BUCKET, R2_ACCESS_KEY_ID)")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, errors.New("R2_ACCOUNT_ID is required without R2_ENDPOINT")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}
	opts := s3.Options{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, ""),
		BaseEndpoint: aws.String(endpoint),
		// R2 rejects virtual-host addressing, and the local stand-in is
		// reached by path style.
		UsePathStyle: true,
	}
	return &R2{
		client:        s3.New(opts),
		presign:       s3.NewPresignClient(s3.New(opts)),
		bucket:        cfg.Bucket,
		publicBaseURL: cfg.PublicBaseURL,
	}, nil
}

// PresignPut signs a PUT of exactly size bytes of contentType. Both are
// signed headers, so a client sending different values is rejected by
// storage. Without the length in the signature anyone could upload 5 GB.
func (r *R2) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, map[string]string, error) {
	out, err := r.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("presign put: %w", err)
	}
	// Only the two headers the client must set: the SDK also signs host,
	// which the HTTP client supplies itself.
	headers := map[string]string{"Content-Type": contentType, "Content-Length": strconv.FormatInt(size, 10)}
	return out.URL, headers, nil
}

// Head returns the stored object's size and content type.
func (r *R2) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := r.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return ObjectInfo{}, fmt.Errorf("head object: %w", err)
	}
	info := ObjectInfo{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}
	return info, nil
}

// Delete removes an object. A missing object is ErrObjectNotFound.
func (r *R2) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(key),
	})
	if err != nil && isNotFound(err) {
		return fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	return err
}

// PublicURL is where a stored object is served from.
func (r *R2) PublicURL(key string) string {
	if r.publicBaseURL == "" {
		return ""
	}
	return r.publicBaseURL + "/" + key
}

// CreateBucket makes the bucket if it is missing (idempotent). Used by the
// local stand-in for dev and tests; R2 buckets are created in the console.
func (r *R2) CreateBucket(ctx context.Context) error {
	_, err := r.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(r.bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// DeleteBucket removes the bucket and everything in it (tests only).
func (r *R2) DeleteBucket(ctx context.Context) error {
	_, err := r.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(r.bucket)})
	if err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}
	return nil
}

// isNotFound reports whether err is S3's NoSuchKey/NotFound.
func isNotFound(err error) bool {
	var nsk *types.NotFound
	var nf *types.NoSuchKey
	var api smithy.APIError
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
