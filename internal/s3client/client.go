package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/sol1/proxs3/internal/config"
)

// ErrNotFound is returned (wrapped) when an object does not exist on S3, so
// callers can distinguish a deleted/missing object from a transport failure
// via errors.Is(err, s3client.ErrNotFound).
var ErrNotFound = errors.New("s3client: object not found")

// isNotFound reports whether err represents an S3 "object does not exist"
// response (HTTP 404). HeadObject returns types.NotFound; S3-compatible
// backends (e.g. MinIO) may surface a generic APIError or a 404 status.
func isNotFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}

// S3Client is the interface used by the API server for S3 operations.
type S3Client interface {
	StorageID() string
	ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error)
	HeadObject(ctx context.Context, key string) (*ObjectInfo, error)
	GetObject(ctx context.Context, key string) (*GetObjectResult, error)
	DownloadToFile(ctx context.Context, key string, w io.WriterAt) (int64, error)
	// PutObject returns the ETag S3 assigned to the new object.
	PutObject(ctx context.Context, key string, body io.Reader, size int64) (string, error)
	DeleteObject(ctx context.Context, key string) error
	CopyObject(ctx context.Context, srcKey, dstKey string) error
	GetObjectTagging(ctx context.Context, key string) (map[string]string, error)
	PutObjectTagging(ctx context.Context, key string, tags map[string]string) error
	HeadBucket(ctx context.Context) error
}

// Client wraps the AWS S3 client for a single storage backend.
type Client struct {
	s3         *s3.Client
	bucket     string
	id         string
	partSizeMB int64
}

// New creates an S3 client from a StorageConfig, with optional proxy support.
func New(cfg config.StorageConfig, proxy config.ProxyConfig) (*Client, error) {
	scheme := "https"
	if !cfg.UseSSL {
		scheme = "http"
	}
	endpoint := fmt.Sprintf("%s://%s", scheme, cfg.Endpoint)

	// Explicit setup/handshake timeouts so we fail fast on connection
	// problems, but no body deadline — large object transfers must be
	// allowed to take as long as they need.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if proxy.HTTPSProxy != "" || proxy.HTTPProxy != "" {
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme == "https" && proxy.HTTPSProxy != "" {
				return url.Parse(proxy.HTTPSProxy)
			}
			if proxy.HTTPProxy != "" {
				return url.Parse(proxy.HTTPProxy)
			}
			return nil, nil
		}
	}
	httpClient := &http.Client{Transport: transport}

	s3Client := s3.New(s3.Options{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "",
		),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: cfg.PathStyle,
		HTTPClient:   httpClient,
	})

	partSizeMB := cfg.PartSizeMB
	if partSizeMB <= 0 {
		partSizeMB = 64 // default: 64MB (AWS SDK default)
	}

	return &Client{
		s3:         s3Client,
		bucket:     cfg.Bucket,
		id:         cfg.StorageID,
		partSizeMB: partSizeMB,
	}, nil
}

// StorageID returns the Proxmox storage identifier.
func (c *Client) StorageID() string {
	return c.id
}

// ObjectInfo represents metadata about an S3 object.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified"`
}

// GetObjectResult contains the object body plus its metadata.
type GetObjectResult struct {
	Body         io.ReadCloser
	Size         int64
	ETag         string
	LastModified time.Time
}

// ListObjects lists objects under a given prefix.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing objects prefix=%s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			objects = append(objects, ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				ETag:         aws.ToString(obj.ETag),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return objects, nil
}

// HeadObject returns metadata about an object without downloading it.
func (c *Client) HeadObject(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("heading object %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("heading object %s: %w", key, err)
	}
	return &ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

// GetObject downloads an object and returns a result with body and metadata.
func (c *Client) GetObject(ctx context.Context, key string) (*GetObjectResult, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("getting object %s: %w", key, err)
	}
	return &GetObjectResult{
		Body:         out.Body,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

// DownloadToFile streams an object into w using the AWS SDK's Downloader,
// which issues parallel Range GETs (4 × 64 MB by default) and retries each
// chunk independently. This is the right primitive for multi-GB objects
// where a single streaming GET is fragile against mid-transfer failures.
func (c *Client) DownloadToFile(ctx context.Context, key string, w io.WriterAt) (int64, error) {
	downloader := manager.NewDownloader(c.s3, func(d *manager.Downloader) {
		d.PartSize = c.partSizeMB * 1024 * 1024
		d.Concurrency = 4
	})
	n, err := downloader.Download(ctx, w, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("downloading object %s: %w", key, err)
	}
	return n, nil
}

// PutObject uploads an object from a reader and returns its ETag.
// Uses multipart upload with part size configured via PartSizeMB (default 64MB).
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64) (string, error) {
	uploader := manager.NewUploader(c.s3, func(u *manager.Uploader) {
		u.PartSize = c.partSizeMB * 1024 * 1024
		u.Concurrency = 4
	})
	out, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	if err != nil {
		return "", fmt.Errorf("putting object %s: %w", key, err)
	}
	return aws.ToString(out.ETag), nil
}

// DeleteObject removes an object from the bucket.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting object %s: %w", key, err)
	}
	return nil
}

// CopyObject copies an object within the same bucket.
func (c *Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		CopySource: aws.String(fmt.Sprintf("%s/%s", c.bucket, srcKey)),
		Key:        aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("copying object %s to %s: %w", srcKey, dstKey, err)
	}
	return nil
}

// GetObjectTagging returns the tags on an S3 object.
func (c *Client) GetObjectTagging(ctx context.Context, key string) (map[string]string, error) {
	out, err := c.s3.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("getting tags for %s: %w", key, err)
	}
	tags := make(map[string]string)
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// PutObjectTagging sets tags on an S3 object (replaces all existing tags).
func (c *Client) PutObjectTagging(ctx context.Context, key string, tags map[string]string) error {
	var tagSet []types.Tag
	for k, v := range tags {
		tagSet = append(tagSet, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := c.s3.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket:  aws.String(c.bucket),
		Key:     aws.String(key),
		Tagging: &types.Tagging{TagSet: tagSet},
	})
	if err != nil {
		return fmt.Errorf("setting tags for %s: %w", key, err)
	}
	return nil
}

// HeadBucket checks if the bucket is reachable (health check).
func (c *Client) HeadBucket(ctx context.Context) error {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	return err
}
