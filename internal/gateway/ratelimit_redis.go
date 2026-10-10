package gateway

import "github.com/omniswitch-dev/omniswitch/internal/limits"

// RedisRateLimitBackend is retained as an alias for internal embedding callers.
type RedisRateLimitBackend = limits.RedisBackend

func NewRedisRateLimitBackend(rawURL, prefix string) (*RedisRateLimitBackend, error) {
	return limits.NewRedisBackend(rawURL, prefix)
}
