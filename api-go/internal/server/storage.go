package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The bucket is the only store. Each asset is one object at {prefix}public/{id}; its
// metadata lives in the object's Content-Type and x-amz-meta-* headers.

type Storage struct {
	client *s3.Client
	bucket string
	prefix string
}

// Asset is the metadata of one stored picture.
type Asset struct {
	ID          string
	Filename    string
	ContentType string
	SizeBytes   int64
	Width       int
	Height      int
	SHA256      string
	CreatedAt   time.Time
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
	return &Storage{client: client, bucket: cfg.Bucket, prefix: cfg.EnvPrefix + "public/"}
}

func (s *Storage) key(id string) string {
	return s.prefix + id
}

func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	var apiErr smithy.APIError
	return errors.As(err, &noSuchKey) || errors.As(err, &notFound) ||
		(errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey"))
}

func assetMetadata(a *Asset) map[string]string {
	return map[string]string{
		"filename": url.PathEscape(a.Filename),
		"width":    strconv.Itoa(a.Width),
		"height":   strconv.Itoa(a.Height),
		"sha256":   a.SHA256,
	}
}

func assetFromObject(id, contentType string, size int64, modified time.Time, meta map[string]string) *Asset {
	a := &Asset{ID: id, ContentType: contentType, SizeBytes: size, CreatedAt: modified, SHA256: meta["sha256"]}
	a.Filename, _ = url.PathUnescape(meta["filename"])
	if a.Filename == "" {
		a.Filename = id
	}
	a.Width, _ = strconv.Atoi(meta["width"])
	a.Height, _ = strconv.Atoi(meta["height"])
	return a
}

func (s *Storage) put(ctx context.Context, a *Asset, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(a.ID)),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(a.ContentType),
		Metadata:      assetMetadata(a),
	})
	if err != nil {
		return fmt.Errorf("put object %q: %w", a.ID, err)
	}
	return nil
}

// head returns (nil, nil) when the asset does not exist.
func (s *Storage) head(ctx context.Context, id string) (*Asset, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(id)),
	})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("head object %q: %w", id, err)
	}
	return assetFromObject(id, aws.ToString(out.ContentType), aws.ToInt64(out.ContentLength), aws.ToTime(out.LastModified), out.Metadata), nil
}

// get returns the asset and an open body, or (nil, nil, nil) when it does not exist.
func (s *Storage) get(ctx context.Context, id string) (*Asset, io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(id)),
	})
	if isNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get object %q: %w", id, err)
	}
	asset := assetFromObject(id, aws.ToString(out.ContentType), aws.ToInt64(out.ContentLength), aws.ToTime(out.LastModified), out.Metadata)
	return asset, out.Body, nil
}

func (s *Storage) delete(ctx context.Context, id string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(id)),
	})
	if err != nil {
		return fmt.Errorf("delete object %q: %w", id, err)
	}
	return nil
}

type listedObject struct {
	ID       string
	Modified time.Time
}

// listIDs returns every asset ID, newest first. Listing doesn't return user metadata, so
// callers HEAD the page they show.
func (s *Storage) listIDs(ctx context.Context) ([]listedObject, error) {
	var objects []listedObject
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			id := strings.TrimPrefix(aws.ToString(obj.Key), s.prefix)
			if isValidID(id) {
				objects = append(objects, listedObject{ID: id, Modified: aws.ToTime(obj.LastModified)})
			}
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		if !objects[i].Modified.Equal(objects[j].Modified) {
			return objects[i].Modified.After(objects[j].Modified)
		}
		return objects[i].ID > objects[j].ID
	})
	return objects, nil
}
