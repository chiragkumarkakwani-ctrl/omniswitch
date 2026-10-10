package limits

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisBackend shares fixed-window counters across gateway replicas. This
// replaces the request-only backend formerly in internal/gateway.
type RedisBackend struct {
	client *redis.Client
	prefix string
}

func NewRedisBackend(rawURL, prefix string) (*RedisBackend, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("rate_limit.redis_url must be a valid Redis connection URL")
	}
	if prefix == "" {
		prefix = "omniswitch:ratelimit:"
	}
	return &RedisBackend{client: redis.NewClient(options), prefix: prefix}, nil
}

func (b *RedisBackend) Ping(ctx context.Context) error { return b.client.Ping(ctx).Err() }
func (b *RedisBackend) Close() error                   { return b.client.Close() }

var consumeScript = redis.NewScript(`
local used = {}
local denied = -1
for i, key in ipairs(KEYS) do
  used[i] = tonumber(redis.call('GET', key) or '0')
  local amount = tonumber(ARGV[(i-1)*3+1])
  local limit = tonumber(ARGV[(i-1)*3+2])
  if denied == -1 and limit > 0 and amount > limit - used[i] then
    denied = i-1
  end
end
if denied == -1 then
  for i, key in ipairs(KEYS) do
    used[i] = math.min(9007199254740991, used[i] + tonumber(ARGV[(i-1)*3+1]))
    redis.call('SET', key, string.format('%.0f', used[i]))
    redis.call('PEXPIREAT', key, ARGV[(i-1)*3+3])
  end
end
local result = {denied}
for i, value in ipairs(used) do result[i+1] = value end
return result
`)

func (b *RedisBackend) Consume(ctx context.Context, charges []Charge) ([]int64, int, error) {
	if len(charges) == 0 {
		return []int64{}, -1, nil
	}
	keys := make([]string, len(charges))
	args := make([]any, 0, len(charges)*3)
	for index, charge := range charges {
		keys[index] = b.prefix + charge.Key
		args = append(args, charge.Amount, charge.Limit, charge.ExpiresAt.UnixMilli())
	}
	values, err := consumeScript.Run(ctx, b.client, keys, args...).Slice()
	if err != nil {
		return nil, -1, err
	}
	if len(values) != len(charges)+1 {
		return nil, -1, fmt.Errorf("unexpected Redis counter result length %d", len(values))
	}
	denied, ok := values[0].(int64)
	if !ok {
		return nil, -1, fmt.Errorf("unexpected Redis denial result %T", values[0])
	}
	used := make([]int64, len(charges))
	for index := range charges {
		value, ok := values[index+1].(int64)
		if !ok {
			return nil, -1, fmt.Errorf("unexpected Redis counter result %T", values[index+1])
		}
		used[index] = value
	}
	return used, int(denied), nil
}

var adjustScript = redis.NewScript(`
for i, key in ipairs(KEYS) do
  local current = redis.call('GET', key)
  if current then
    local value = math.max(0, math.min(9007199254740991, tonumber(current) + tonumber(ARGV[i])))
    redis.call('SET', key, string.format('%.0f', value), 'KEEPTTL')
  end
end
return 1
`)

func (b *RedisBackend) Adjust(ctx context.Context, charges []Charge) error {
	if len(charges) == 0 {
		return nil
	}
	keys := make([]string, len(charges))
	args := make([]any, len(charges))
	for index, charge := range charges {
		keys[index], args[index] = b.prefix+charge.Key, charge.Amount
	}
	return adjustScript.Run(ctx, b.client, keys, args...).Err()
}

func (b *RedisBackend) Counts(ctx context.Context, keys []string) ([]int64, error) {
	used := make([]int64, len(keys))
	if len(keys) == 0 {
		return used, nil
	}
	fullKeys := make([]string, len(keys))
	for index, key := range keys {
		fullKeys[index] = b.prefix + key
	}
	values, err := b.client.MGet(ctx, fullKeys...).Result()
	if err != nil {
		return nil, err
	}
	for index, value := range values {
		if value == nil {
			continue
		}
		used[index], err = strconv.ParseInt(fmt.Sprint(value), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse Redis counter: %w", err)
		}
	}
	return used, nil
}

func (b *RedisBackend) List(ctx context.Context, prefix string) (map[string]int64, error) {
	iterator := b.client.Scan(ctx, 0, b.prefix+prefix+"*", 100).Iterator()
	var keys []string
	for iterator.Next(ctx) {
		keys = append(keys, strings.TrimPrefix(iterator.Val(), b.prefix))
	}
	if err := iterator.Err(); err != nil {
		return nil, err
	}
	values, err := b.Counts(ctx, keys)
	if err != nil {
		return nil, err
	}
	used := make(map[string]int64, len(keys))
	for index, key := range keys {
		used[key] = values[index]
	}
	return used, nil
}
