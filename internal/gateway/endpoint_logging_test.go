package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/guardrail"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
)

func TestRequestLogEndpoints(t *testing.T) {
	for _, test := range []struct {
		path, body, endpoint string
		serve                func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, "chat_completions", (*Handler).ChatCompletions},
		{"/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "chat_completions", (*Handler).ChatCompletions},
		{"/v1/responses", `{"model":"test-model","input":"hi"}`, "responses", (*Handler).Responses},
		{"/v1/messages", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`, "messages", (*Handler).Messages},
		{"/v1/embeddings", `{"model":"embedding-model","input":"hi"}`, "embeddings", (*Handler).Embeddings},
		{"/v1/rerank", `{"model":"rerank-model","query":"hi","documents":["hi"]}`, "rerank", (*Handler).Rerank},
		{"/a2a", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"parts":[{"text":"hi"}]},"metadata":{"model":"test-model"}}}`, "a2a", (*Handler).A2A},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			st := newGatewayTestStore(t)
			registry := provider.NewRegistry()
			registry.Register(gatewayProvider{name: "test", model: "test-model"})
			registry.Register(embeddingGatewayProvider{})
			registry.Register(rerankGatewayProvider{})
			handler := New(registry, router.New(registry), st, guardrail.NewEngine())
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			rec := httptest.NewRecorder()
			test.serve(handler, rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("request failed: %d: %s", rec.Code, rec.Body.String())
			}
			logs, _, err := st.ListLogs(context.Background(), 10, 0, "", "")
			if err != nil || len(logs) != 1 || logs[0].Endpoint != test.endpoint {
				t.Fatalf("request endpoint logs = %+v, err=%v, want %q", logs, err, test.endpoint)
			}
		})
	}
}
