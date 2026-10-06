package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const presignLifetime = 15 * time.Minute

func newS3Client(cfg S3Config) *s3.Client {
	return s3.New(s3.Options{
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
}

type storedObject struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

func fetchObject(ctx context.Context, client *s3.Client, bucket, key string) (storedObject, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return storedObject{}, fmt.Errorf("get object %q: %w", key, err)
	}

	contentType := contentTypeForKey(key)
	if contentType == "" && out.ContentType != nil {
		contentType = *out.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	return storedObject{
		Body:        out.Body,
		ContentType: contentType,
		Size:        size,
	}, nil
}

func contentTypeForKey(key string) string {
	return mime.TypeByExtension(path.Ext(key))
}

func presignGetObjectURL(ctx context.Context, client *s3.Client, bucket, key string) (string, error) {
	input := &s3.GetObjectInput{
		Bucket:                     aws.String(bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String("inline"),
	}
	if contentType := contentTypeForKey(key); contentType != "" {
		input.ResponseContentType = aws.String(contentType)
	}

	presigned, err := s3.NewPresignClient(client).PresignGetObject(ctx, input, s3.WithPresignExpires(presignLifetime))
	if err != nil {
		return "", fmt.Errorf("presign get object %q: %w", key, err)
	}
	return presigned.URL, nil
}
