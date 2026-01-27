package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisLock struct {
	client *redis.Client
	key    string
	token  string
}

func NewRedisClient(addr, password string, db int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}

func TryLock(ctx context.Context, client *redis.Client, key string, ttl time.Duration) (*RedisLock, bool, error) {
	token, err := newToken()
	if err != nil {
		return nil, false, err
	}
	ok, err := client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &RedisLock{client: client, key: key, token: token}, true, nil
}

func (l *RedisLock) Unlock(ctx context.Context) error {
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`
	_, err := l.client.Eval(ctx, script, []string{l.key}, l.token).Result()
	return err
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
