package cache

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestIsNotFoundRecognizesHeadAndGetErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "get object", err: &types.NoSuchKey{}},
		{name: "head object", err: &types.NotFound{}},
		{name: "wrapped head object", err: errors.Join(errors.New("head failed"), &types.NotFound{})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isNotFound(tt.err) {
				t.Fatalf("isNotFound(%T) = false", tt.err)
			}
		})
	}
}

func TestResponseHeadersMetadataRoundTrip(t *testing.T) {
	headers := map[string]string{
		"Cache-Control":           "public, max-age=60",
		"Content-Security-Policy": "default-src 'self'",
	}
	metadata := map[string]string{
		responseHeadersMetaKey: encodeResponseHeaders(headers),
	}

	if got := parseResponseHeaders(metadata); !reflect.DeepEqual(got, headers) {
		t.Fatalf("headers = %#v, want %#v", got, headers)
	}
}
