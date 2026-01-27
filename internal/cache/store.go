package cache

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("cache object not found")

type Object struct {
	Body        []byte
	ContentType string
	Encoding    string
	UpdatedAt   time.Time
}

type Store interface {
	Get(ctx context.Context, key string) (Object, error)
	Put(ctx context.Context, key string, obj Object) error
	UpdatedAt(ctx context.Context, key string) (time.Time, error)
	Delete(ctx context.Context, key string) error
}
