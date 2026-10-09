package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/adapter/mcp"
)

const toolListCacheTTL = 30 * time.Second

type toolListCache struct {
	mu        sync.Mutex
	key       [32]byte
	response  []byte
	expiresAt time.Time
}

func (c *toolListCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.response = nil
	c.expiresAt = time.Time{}
}

// upstreamToolList caches unfiltered discovery, never a policy decision. Each
// caller's tools are filtered again. The bounded per-target entry is partitioned
// by session, pagination parameters, and delegated credentials so one subject's
// upstream inventory cannot be served to another subject.
func (h *Handler) upstreamToolList(ctx context.Context, target target, body []byte, headers http.Header) ([]map[string]any, error) {
	var request struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	delegatedToken := ""
	if target.forwardBearerToken || target.tokenExchange != nil {
		delegatedToken = headers.Get("Authorization")
	}
	key := sha256.Sum256([]byte(strings.Join([]string{
		string(request.Params), headers.Get("Mcp-Session-Id"), headers.Get("Mcp-Protocol-Version"), delegatedToken,
	}, "\x00")))
	cache := target.toolsCache
	cache.mu.Lock()
	defer cache.mu.Unlock()

	response := cache.response
	if response == nil || cache.key != key || !time.Now().Before(cache.expiresAt) {
		status, _, payload, err := h.callTarget(ctx, target, body, headers)
		if err != nil {
			return nil, err
		}
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("upstream tools/list returned HTTP %d", status)
		}
		response = payload
	}
	var result struct {
		Result *struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *mcp.Error `json:"error"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return nil, fmt.Errorf("decode upstream tools/list: %w", err)
	}
	if result.Result == nil || result.Error != nil {
		return nil, fmt.Errorf("upstream tools/list did not return a result")
	}
	if cache.key != key || cache.response == nil || !time.Now().Before(cache.expiresAt) {
		cache.key = key
		cache.response = response
		cache.expiresAt = time.Now().Add(toolListCacheTTL)
	}
	return result.Result.Tools, nil
}
