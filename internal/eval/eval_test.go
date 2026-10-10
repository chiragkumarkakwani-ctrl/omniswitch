package eval

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/model"
)

func TestReplayPoliciesNamedAndInline(t *testing.T) {
	dir := t.TempDir()
	body := `// name: production-delete
// reason: Production deletes are blocked.
resource.environment == "production" && action.name == "delete"`
	if err := os.WriteFile(filepath.Join(dir, "production-delete.cel"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, inline := range []bool{false, true} {
		source := policySource{Name: "production-delete.cel"}
		if inline {
			source.Body = body
		}
		rec := replayRequest(t, NewHandler(dir), policyReplayRequest{
			Policies: []policySource{source}, Requests: requestsFor("delete", "read"),
		})
		response := decodeReplay(t, rec)
		if response.Total != 2 || response.Allowed != 1 || response.Denied != 1 || response.Errors != 0 {
			t.Fatalf("inline=%v: counters = %+v", inline, response.policyReplaySummary)
		}
		if response.Results[0].Decision.Allowed || !response.Results[1].Decision.Allowed {
			t.Fatalf("unexpected replay decisions: %+v", response.Results)
		}
		if response.Results[0].Decision.Reason != "Production deletes are blocked." {
			t.Fatalf("CEL metadata lost: %+v", response.Results[0].Decision)
		}
		if strings.Contains(rec.Body.String(), dir) {
			t.Fatal("replay disclosed the server policy directory")
		}
	}
}

func TestReplayPoliciesInlineYAML(t *testing.T) {
	body := `apiVersion: omniswitch.dev/v1
kind: Policy
metadata:
  name: block-delete
spec:
  effect: deny
  reason: Deletes are blocked.
  cel: action.name == "delete"
`
	rec := replayRequest(t, NewHandler(filepath.Join(t.TempDir(), "missing")), policyReplayRequest{
		Policies: []policySource{{Name: "block-delete.yaml", Body: body}}, Requests: requestsFor("delete", "read"),
	})
	response := decodeReplay(t, rec)
	if response.Denied != 1 || response.Allowed != 1 || response.Results[0].Decision.Rule != "block-delete" {
		t.Fatalf("YAML replay = %+v", response)
	}
}

func TestReplayPoliciesRejectsPaths(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "policies")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.cel")
	if err := os.WriteFile(outside, []byte(`true`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.cel")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"../outside.cel", outside, filepath.Join(dir, "inside.cel"), "/etc/passwd",
		"nested/policy.cel", `..\outside.cel`, `C:\policies\policy.cel`, "escape.cel",
	} {
		t.Run(name, func(t *testing.T) {
			rec := replayRequest(t, NewHandler(dir), policyReplayRequest{
				Policies: []policySource{{Name: name}}, Requests: requestsFor("delete"),
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("path %q status = %d, body = %s", name, rec.Code, rec.Body.String())
			}
		})
	}
	// Baselines use the same confined loader.
	rec := replayRequest(t, NewHandler(dir), policyReplayRequest{
		Policies:         []policySource{{Name: "candidate", Body: "false"}},
		BaselinePolicies: []policySource{{Name: "../outside.cel"}}, Requests: requestsFor("read"),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("baseline traversal status = %d", rec.Code)
	}
}

func TestReplayPoliciesServerSideBaselineDiff(t *testing.T) {
	rec := replayRequest(t, NewHandler(t.TempDir()), policyReplayRequest{
		Policies:         []policySource{{Name: "rules.cel", Body: `action.name == "write"`}},
		BaselinePolicies: []policySource{{Name: "rules.cel", Body: `action.name == "delete"`}},
		Requests:         requestsFor("delete", "read", "write"),
	})
	response := decodeReplay(t, rec)
	if response.Baseline == nil || response.Baseline.Total != 3 || response.Baseline.Denied != 1 {
		t.Fatalf("missing server baseline: %+v", response.Baseline)
	}
	if response.Diff == nil || response.Diff.Changed != 2 || response.Diff.AllowedToDenied != 1 || response.Diff.DeniedToAllowed != 1 {
		t.Fatalf("unexpected decision diff: %+v", response.Diff)
	}
	if len(response.Diff.Changes) != 2 || response.Diff.Changes[0].Index != 0 || response.Diff.Changes[1].Index != 2 {
		t.Fatalf("unexpected change indexes: %+v", response.Diff.Changes)
	}
}

func TestReplayPoliciesValidation(t *testing.T) {
	for _, body := range []string{
		`{"requests":[]}`, `{not-json`, `{} {}`, `{"policy_paths":["/etc/passwd"],"requests":[{}]}`,
		`{"policies":[{"name":"deny","body":"false"}],"requests":[{}],"baseline":{"allowed":1}}`,
		`{"policies":[{"name":"deny","body":"false"}],"requests":[]}`,
		`{"policies":[{"name":"bad","body":"this is not valid CEL"}],"requests":[{}]}`,
		`{"policies":[{"name":"unsupported.txt"}],"requests":[{}]}`,
	} {
		rec := httptest.NewRecorder()
		NewHandler(t.TempDir()).ReplayPolicies(rec, httptest.NewRequest(http.MethodPost, "/api/evals/policy", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, rec.Code)
		}
	}
}

func TestReplayPoliciesItemLimit(t *testing.T) {
	for _, count := range []int{maxReplayItems, maxReplayItems + 1} {
		requests := make([]model.ToolRequest, count)
		rec := replayRequest(t, NewHandler(t.TempDir()), policyReplayRequest{
			Policies: []policySource{{Name: "deny.cel", Body: "false"}}, Requests: requests,
		})
		want := http.StatusOK
		if count > maxReplayItems {
			want = http.StatusBadRequest
		}
		if rec.Code != want {
			t.Fatalf("count %d status = %d, want %d: %s", count, rec.Code, want, rec.Body.String())
		}
	}
}

func TestReplayPoliciesBodyLimit(t *testing.T) {
	body := `{"policies":[{"name":"deny.cel","body":"false"}],"requests":[{}]}`
	for _, size := range []int{maxReplayBytes, maxReplayBytes + 1} {
		padded := body + strings.Repeat(" ", size-len(body))
		rec := httptest.NewRecorder()
		NewHandler(t.TempDir()).ReplayPolicies(rec, httptest.NewRequest(http.MethodPost, "/api/evals/policy", strings.NewReader(padded)))
		want := http.StatusOK
		if size > maxReplayBytes {
			want = http.StatusRequestEntityTooLarge
		}
		if rec.Code != want {
			t.Fatalf("body size %d status = %d, want %d: %s", size, rec.Code, want, rec.Body.String())
		}
	}
}

func TestReplayPoliciesLimitsFileBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "large.cel"), []byte(strings.Repeat(" ", maxReplayBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := replayRequest(t, NewHandler(dir), policyReplayRequest{
		Policies: []policySource{{Name: "large.cel"}}, Requests: requestsFor("read"),
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "exceed 1 MiB") {
		t.Fatalf("large file policy status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func requestsFor(actions ...string) []model.ToolRequest {
	requests := make([]model.ToolRequest, len(actions))
	for index, action := range actions {
		requests[index].Action.Name = action
		requests[index].Resource.Environment = "production"
	}
	return requests
}

func replayRequest(t *testing.T, handler *Handler, request policyReplayRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ReplayPolicies(rec, httptest.NewRequest(http.MethodPost, "/api/evals/policy", strings.NewReader(string(body))))
	return rec
}

func decodeReplay(t *testing.T, rec *httptest.ResponseRecorder) policyReplayResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("ReplayPolicies status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response policyReplayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestReplayPoliciesIdenticalBaselineIgnoresIDsAndTiming(t *testing.T) {
	policies := []policySource{{Name: "deny.cel", Body: `false`}}
	rec := replayRequest(t, NewHandler(t.TempDir()), policyReplayRequest{
		Policies: policies, BaselinePolicies: policies, Requests: requestsFor("read"),
	})
	response := decodeReplay(t, rec)
	if response.Diff == nil || response.Diff.Changed != 0 || len(response.Diff.Changes) != 0 {
		t.Fatalf("identical policy produced changes: %+v", response.Diff)
	}
}
