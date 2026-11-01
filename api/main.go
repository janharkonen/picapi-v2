// Command picapi serves objects from an S3-compatible bucket over HTTP.
// Every request is fetched from the bucket and streamed to the client;
// nothing is cached.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gin-gonic/gin"
)

func main() {
	bucket := mustEnv("S3_BUCKET")
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(mustEnv("S3_ENDPOINT")),
		Region:       envOr("S3_REGION", "auto"),
		Credentials:  credentials.NewStaticCredentialsProvider(mustEnv("S3_ACCESS_KEY_ID"), mustEnv("S3_SECRET_ACCESS_KEY"), ""),
		UsePathStyle: os.Getenv("S3_USE_PATH_STYLE") == "true",
	})

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.GET("/*key", func(c *gin.Context) { serveObject(c, client, bucket) })

	addr := ":" + envOr("PORT", "8080")
	log.Printf("picapi listening on %s, bucket %s", addr, bucket)
	log.Fatal(r.Run(addr))
}

// serveObject streams the object named by the request path from the bucket.
func serveObject(c *gin.Context, client *s3.Client, bucket string) {
	key := strings.TrimPrefix(c.Param("key"), "/")
	if key == "" {
		c.String(http.StatusNotFound, "not found")
		return
	}
	obj, err := client.GetObject(c.Request.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noKey *types.NoSuchKey
		if errors.As(err, &noKey) {
			c.String(http.StatusNotFound, "not found")
			return
		}
		if !errors.Is(err, context.Canceled) {
			log.Printf("get %q: %v", key, err)
		}
		c.String(http.StatusBadGateway, "storage error")
		return
	}
	defer obj.Body.Close()

	headers := map[string]string{}
	if obj.ETag != nil {
		headers["ETag"] = *obj.ETag
	}
	if obj.LastModified != nil {
		headers["Last-Modified"] = obj.LastModified.UTC().Format(http.TimeFormat)
	}
	length := int64(-1)
	if obj.ContentLength != nil {
		length = *obj.ContentLength
		headers["Content-Length"] = strconv.FormatInt(length, 10)
	}
	c.DataFromReader(http.StatusOK, length, aws.ToString(obj.ContentType), obj.Body, headers)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("missing required environment variable %s", name)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
