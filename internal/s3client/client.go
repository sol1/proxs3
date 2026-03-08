package s3client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sol1/proxs3/internal/config"
)

// S3Client is the interface used by the API server for S3 operations.
type S3Client interface {
	StorageID() string
	ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error)
	HeadObject(ctx context.Context, key string) (*ObjectInfo, error)
	GetObject(ctx context.Context, key string) (*GetObjectResult, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64) error
	DeleteObject(ctx context.Context, key string) error
	HeadBucket(ctx context.Context) error
}

// Client wraps the AWS S3 client for a single storage backend.
type Client struct {
	s3     *s3.Client
	bucket string
	id     string
}

// New creates an S3 client from a StorageConfig, with optional proxy support.
func New(cfg config.StorageConfig, proxy config.ProxyConfig) (*Client, error) {
	scheme := "https"
	if !cfg.UseSSL {
		scheme = "http"
	}
	endpoint := fmt.Sprintf("%s://%s", scheme, cfg.Endpoint)

	transport := &http.Transport{}
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

	return &Client{
		s3:     s3Client,
		bucket: cfg.Bucket,
		id:     cfg.StorageID,
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

// PutObject uploads an object from a reader.
// Uses multipart upload automatically for files larger than 64MB.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64) error {
	uploader := manager.NewUploader(c.s3, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024 // 64MB per part
		u.Concurrency = 4
	})
	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("putting object %s: %w", key, err)
	}
	return nil
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

// HeadBucket checks if the bucket is reachable (health check).
func (c *Client) HeadBucket(ctx context.Context) error {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	return err
}
