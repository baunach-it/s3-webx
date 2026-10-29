package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type Bucket struct {
	client *minio.Client
	bucket string
	prefix string
}

// An endpoint without scheme is treated as https; minio-go itself wants host and TLS flag apart.
func New(
	endpoint string,
	region string,
	bucket string,
	prefix string,
	pathStyle bool,
	accessKey string,
	secretKey string,
) (*Bucket, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q", endpoint)
	}
	lookup := minio.BucketLookupDNS
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	return &Bucket{client: client, bucket: bucket, prefix: prefix}, nil
}

func (b *Bucket) List(ctx context.Context) ([]Object, error) {
	var objects []Object
	for info := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: b.prefix, Recursive: true}) {
		if info.Err != nil {
			return nil, fmt.Errorf("list %s: %w", b.bucket, info.Err)
		}
		if strings.HasSuffix(info.Key, "/") {
			continue
		}
		objects = append(objects, Object{Key: info.Key, Size: info.Size, LastModified: info.LastModified})
	}
	return objects, nil
}

func (b *Bucket) Download(ctx context.Context, key, path string) error {
	if err := b.client.FGetObject(ctx, b.bucket, key, path, minio.GetObjectOptions{}); err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	return nil
}

func (b *Bucket) Upload(
	ctx context.Context,
	key string,
	path string,
	contentType string,
	cacheControl string,
) error {
	_, err := b.client.FPutObject(ctx, b.bucket, key, path, minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: cacheControl,
	})
	if err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

func (b *Bucket) CacheControl(ctx context.Context, key string) (string, error) {
	info, err := b.client.StatObject(ctx, b.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", key, err)
	}
	return info.Metadata.Get("Cache-Control"), nil
}
