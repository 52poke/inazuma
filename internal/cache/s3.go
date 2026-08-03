package cache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const updatedAtMetaKey = "updated_at"
const responseHeadersMetaKey = "response_headers"

type S3Store struct {
	bucket   string
	client   *s3.Client
	uploader *manager.Uploader
}

func NewS3Store(bucket string, client *s3.Client) *S3Store {
	return &S3Store{
		bucket:   bucket,
		client:   client,
		uploader: manager.NewUploader(client),
	}
}

func (s *S3Store) Get(ctx context.Context, key string) (Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}
	defer out.Body.Close()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return Object{}, err
	}

	return Object{
		Body:        body,
		ContentType: aws.ToString(out.ContentType),
		Encoding:    aws.ToString(out.ContentEncoding),
		Headers:     parseResponseHeaders(out.Metadata),
		UpdatedAt:   parseUpdatedAt(out.Metadata),
	}, nil
}

func (s *S3Store) UpdatedAt(ctx context.Context, key string) (time.Time, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, err
	}
	return parseUpdatedAt(out.Metadata), nil
}

func (s *S3Store) Put(ctx context.Context, key string, obj Object) error {
	meta := map[string]string{}
	if !obj.UpdatedAt.IsZero() {
		meta[updatedAtMetaKey] = strconv.FormatInt(obj.UpdatedAt.Unix(), 10)
	}
	if encoded := encodeResponseHeaders(obj.Headers); encoded != "" {
		meta[responseHeadersMetaKey] = encoded
	}

	input := &s3.PutObjectInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(obj.Body),
		ContentType:     aws.String(obj.ContentType),
		ContentEncoding: aws.String(obj.Encoding),
		Metadata:        meta,
	}

	_, err := s.uploader.Upload(ctx, input)
	return err
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

func parseUpdatedAt(meta map[string]string) time.Time {
	if meta == nil {
		return time.Time{}
	}
	val, ok := meta[updatedAtMetaKey]
	if !ok {
		return time.Time{}
	}
	unix, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func encodeResponseHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	data, err := json.Marshal(headers)
	if err != nil {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(data)
}

func parseResponseHeaders(meta map[string]string) map[string]string {
	if meta == nil || meta[responseHeadersMetaKey] == "" {
		return nil
	}
	data, err := base64.RawStdEncoding.DecodeString(meta[responseHeadersMetaKey])
	if err != nil {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal(data, &headers); err != nil {
		return nil
	}
	return headers
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	return false
}
