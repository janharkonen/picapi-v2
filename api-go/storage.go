package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Storage struct {
	client *s3.Client
	bucket string
	prefix string
}

type storedObject struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64 // -1 when unknown
}

func newStorage(cfg S3Config) *Storage {
	client := s3.New(s3.Options{
		Region: cfg.Region,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		),
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.UsePathStyle,
		// R2 rejects the default flexible checksum headers the SDK adds.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Storage{client: client, bucket: cfg.Bucket, prefix: cfg.EnvPrefix}
}

func (s *Storage) originalKey(id string) string {
	return s.prefix + "originals/" + id
}

func (s *Storage) variantPrefix(id string) string {
	return s.prefix + "cached/" + id + "/"
}

func (s *Storage) variantKey(id, hash, ext string) string {
	return s.variantPrefix(id) + hash + "." + ext
}

func (s *Storage) put(ctx context.Context, key string, body []byte, contentType string, metadata map[string]string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
		Metadata:      metadata,
	})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// get returns (nil, nil) when the object does not exist.
func (s *Storage) get(ctx context.Context, key string) (*storedObject, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, nil
		}
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	obj := &storedObject{Body: out.Body, ContentType: "application/octet-stream", Size: -1}
	if out.ContentType != nil {
		obj.ContentType = *out.ContentType
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	return obj, nil
}

func (s *Storage) getBytes(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.get(ctx, key)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("get object %q: not found", key)
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

func (s *Storage) delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// deletePrefix deletes objects one by one; DeleteObjects needs Content-MD5, which some providers handle poorly.
func (s *Storage) deletePrefix(ctx context.Context, prefix string) error {
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if err := s.delete(ctx, aws.ToString(obj.Key)); err != nil {
				return err
			}
		}
	}
	return nil
}
