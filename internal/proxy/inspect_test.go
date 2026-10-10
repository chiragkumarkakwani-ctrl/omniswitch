package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/audit"
	"github.com/omniswitch-dev/omniswitch/internal/model"
	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

type inspectionEngine func(context.Context, model.ToolRequest) (model.Decision, error)

func (e inspectionEngine) Evaluate(ctx context.Context, request model.ToolRequest) (model.Decision, error) {
	return e(ctx, request)
}

func TestMCPInspectionUsesPoliciesWithoutCallsOrAudits(t *testing.T) {
	engine, err := policy.NewEngine(
		policy.Rule{Name: "hide-delete", Expression: `tool.name == "sql_delete" && action.name == "list" && agent.role != "admin"`, Reason: "Hidden delete"},
		policy.Rule{Name: "legacy-delete", Expression: `tool.name == "sql" && action.name == "delete"`, Reason: "Legacy delete policy"},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Method string `json:"method"` }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "tools/list" {
			t.Errorf("inspection made a tool call: %s/%v", request.Method, err)
		}
		if r.Header.Get("Mcp-Session-Id") != "verified-session" {
			t.Error("discovery dropped MCP session")
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"weather"},{"name":"sql_delete"}]}}`))
	}))
	defer server.Close()
	var events bytes.Buffer
	h, err := NewMultiHandler(engine, audit.NewStdoutLogger(&events), []TargetConfig{{Name: "ops", Upstream: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	headers := http.Header{"Mcp-Session-Id": []string{"verified-session"}}
	agent := model.Agent{ID: "verified-key", Role: "viewer", Department: "workspace"}
	tools, err := h.InspectTools(context.Background(), headers, agent, "ops", false)
	if err != nil || len(tools) != 1 || tools[0].Name != "weather" || !tools[0].Call.EvaluatedWithoutArguments {
		t.Fatalf("visible tools = %+v/%v", tools, err)
	}
	tools, err = h.InspectTools(context.Background(), headers, agent, "OPS", true)
	if err != nil || len(tools) != 2 || tools[1].List.Allowed || tools[1].List.Rule != "hide-delete" || tools[1].Call.Allowed || tools[1].Call.Rule != "legacy-delete" || tools[1].NamespacedName != "ops__sql_delete" {
		t.Fatalf("admin explanations = %+v/%v", tools, err)
	}
	targets := h.InspectTargets(context.Background(), headers, agent)
	if len(targets) != 1 || !targets[0].Connected || targets[0].ToolCount != 2 || targets[0].Visible != 1 || targets[0].Hidden != 1 {
		t.Fatalf("target counts = %+v", targets)
	}
	if events.Len() != 0 {
		t.Fatal("inspection wrote audit events")
	}
	if _, err := h.InspectTools(context.Background(), headers, agent, "unknown", true); !errors.Is(err, ErrInspectionTargetNotFound) {
		t.Fatalf("unknown target error = %v", err)
	}
}

func TestMCPInspectionSyntheticToolsAndSecretFreeFailure(t *testing.T) {
	engine := inspectionEngine(func(_ context.Context, req model.ToolRequest) (model.Decision, error) {
		if req.Tool.Name != "create_ticket" || req.Session.ID != "verified-session" || req.Metadata["mcp.target"] != "api" {
			t.Errorf("inspection lost principal/policy shape: %+v", req)
		}
		if req.Action.Name == "call" {
			if req.Metadata["mcp.openapi"] != "POST /tickets" || req.Resource.Name != "" {
				t.Errorf("synthetic dry run invented arguments/resource: %+v", req)
			}
			return model.Decision{}, errors.New("PRIVATE_POLICY_ERROR")
		}
		return model.Decision{Allowed: true}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("PRIVATE_UPSTREAM_BODY"))
	}))
	defer server.Close()
	spec := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(spec, []byte(`{"openapi":"3.0.0","paths":{"/tickets":{"post":{"operationId":"create_ticket","summary":"Create a ticket","responses":{"200":{"description":"OK"}}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	h, err := NewMultiHandler(engine, audit.NewStdoutLogger(&events), []TargetConfig{{Name: "api", Upstream: server.URL, OpenAPISpec: spec, Headers: map[string]string{"X-Private": "PRIVATE_TARGET_HEADER"}, TokenExchange: &TokenExchangeConfig{TokenURL: "https://PRIVATE_USER:PRIVATE_PASSWORD@auth.example.test/token?secret=PRIVATE_URL_QUERY", ClientID: "PRIVATE_CLIENT", ClientSecret: "PRIVATE_CLIENT_SECRET", ActorToken: "PRIVATE_ACTOR", Audience: "tickets-api"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	headers := http.Header{"Mcp-Session-Id": []string{"verified-session"}}
	targets := h.InspectTargets(context.Background(), headers, model.Agent{ID: "verified-key", Role: "member"})
	if len(targets) != 1 || targets[0].Connected || !targets[0].OpenAPISpec || targets[0].Visible != 1 || targets[0].ToolCount != 1 || !strings.Contains(targets[0].LastError, "known OpenAPI tools only") || !targets[0].TokenExchange.Configured || targets[0].TokenExchange.TokenURLHost != "auth.example.test" || targets[0].TokenExchange.Audience != "tickets-api" {
		t.Fatalf("synthetic target = %+v", targets)
	}
	tools, err := h.InspectTools(context.Background(), headers, model.Agent{ID: "verified-key", Role: "member"}, "api", false)
	if err != nil || len(tools) != 1 || tools[0].Call.Allowed || tools[0].Call.Reason != "policy could not be evaluated without arguments" || !tools[0].Call.EvaluatedWithoutArguments {
		t.Fatalf("synthetic tools = %+v/%v", tools, err)
	}
	encoded, err := json.Marshal(map[string]any{"targets": targets, "tools": tools})
	if err != nil || strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "client_secret") || events.Len() != 0 {
		t.Fatal("inspection exposed private data or wrote an audit")
	}
}

func TestMCPInspectionRefusesPartialAndOversizedDiscovery(t *testing.T) {
	engine, err := policy.NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"message":"PRIVATE_RPC_ERROR"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"partial"}],"nextCursor":"more"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[]},"padding":"` + strings.Repeat("x", 1<<20) + `"}`,
		`not-json`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		h, err := NewHandler(engine, nil, server.URL)
		if err != nil {
			t.Fatal(err)
		}
		targets := h.InspectTargets(context.Background(), nil, model.Agent{})
		if len(targets) != 1 || targets[0].Connected || targets[0].ToolCount != 0 || targets[0].LastError != "tool discovery failed" {
			t.Fatalf("partial discovery reported complete: %+v", targets)
		}
		if _, err := h.InspectTools(context.Background(), nil, model.Agent{}, "", true); err == nil || strings.Contains(err.Error(), "PRIVATE_") {
			t.Fatalf("partial tools/error = %v", err)
		}
		h.Close()
		server.Close()
	}
}
