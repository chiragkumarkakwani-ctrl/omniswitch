package proxy

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/omniswitch-dev/omniswitch/internal/adapter/mcp"
	"github.com/omniswitch-dev/omniswitch/internal/audit"
	"github.com/omniswitch-dev/omniswitch/internal/model"
)

// handleOpenAPICall executes a synthetic OpenAPI-derived tool: policy gate,
// audit event, HTTP execution, and MCP-formatted response.
func (h *Handler) handleOpenAPICall(w http.ResponseWriter, r *http.Request, tgt target, tool *openAPITool, body []byte, requestID json.RawMessage) {
	var parsed struct {
		Params struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeJSON(w, http.StatusBadRequest, mcp.ErrorResponse(requestID, -32700, "Parse error", err))
		return
	}

	rpcReq := mcp.Request{JSONRPC: "2.0", ID: requestID, Method: "tools/call", Params: mcp.ToolCallParams{Name: tool.Name, Arguments: parsed.Params.Arguments}}
	toolReq, err := rpcReq.ToToolRequest()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, mcp.ErrorResponse(requestID, -32602, "Invalid params", nil))
		return
	}
	toolReq.Agent = requestIdentity(r)
	toolReq.Session = model.Session{ID: r.Header.Get("Mcp-Session-Id")}
	toolReq.Tool.Name = tool.Name
	toolReq.Action.Name = "call"
	toolReq.Metadata["mcp.target"] = tgt.name
	toolReq.Metadata["mcp.tool"] = tool.Name
	toolReq.Metadata["mcp.openapi"] = tool.Method + " " + tool.Path
	decision, evalErr := tgt.engine.Evaluate(r.Context(), toolReq)
	if h.auditor != nil {
		_ = h.auditor.Log(r.Context(), audit.NewEvent(toolReq, decision))
	}
	if evalErr != nil || !decision.Allowed {
		writeJSON(w, http.StatusOK, mcp.DeniedResponse(requestID, decision))
		return
	}

	baseURL := ""
	if tgt.upstream != nil {
		baseURL = tgt.upstream.String()
	}
	headers := cloneHeaders(tgt.headers)
	if tgt.tokenExchange != nil {
		token, err := h.getExchangeToken(r.Context(), tgt)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, mcp.ErrorResponse(requestID, -32000, "Token exchange failed", nil))
			return
		}
		if headers == nil {
			headers = make(map[string]string)
		}
		headers["Authorization"] = "Bearer " + token
	}
	resp, err := executeOpenAPICall(h.client, baseURL, headers, tool, parsed.Params.Arguments)
	if err != nil {
		writeJSON(w, http.StatusOK, mcp.Response{JSONRPC: "2.0", ID: requestID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": "upstream error: " + err.Error()}},
			"isError": true,
		}})
		return
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	isError := resp.StatusCode >= 400
	text := string(payload)
	if text == "" {
		text = resp.Status
	}
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
	writeJSON(w, http.StatusOK, mcp.Response{JSONRPC: "2.0", ID: requestID, Result: result})
}
