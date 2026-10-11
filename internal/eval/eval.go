// Package eval replays authorization policies against uploaded tool requests.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/model"
	"github.com/omniswitch-dev/omniswitch/internal/policy"
)

const (
	maxReplayItems = 1000
	maxReplayBytes = 1 << 20
)

type Handler struct {
	policyDir string
}

func NewHandler(policyDir string) *Handler {
	if policyDir == "" {
		policyDir = "policies"
	}
	return &Handler{policyDir: policyDir}
}

type policySource struct {
	Name string `json:"name"`
	Body string `json:"body,omitempty"`
}

type policyReplayRequest struct {
	Policies         []policySource      `json:"policies"`
	BaselinePolicies []policySource      `json:"baseline_policies,omitempty"`
	Requests         []model.ToolRequest `json:"requests"`
}

type policyReplayResult struct {
	Index    int               `json:"index"`
	Request  model.ToolRequest `json:"request"`
	Decision model.Decision    `json:"decision"`
	Error    string            `json:"error,omitempty"`
}

type policyReplaySummary struct {
	Total   int                  `json:"total"`
	Allowed int                  `json:"allowed"`
	Denied  int                  `json:"denied"`
	Errors  int                  `json:"errors"`
	Results []policyReplayResult `json:"results"`
}

type policyReplayChange struct {
	Index       int            `json:"index"`
	Before      model.Decision `json:"before"`
	After       model.Decision `json:"after"`
	BeforeError string         `json:"before_error,omitempty"`
	AfterError  string         `json:"after_error,omitempty"`
}

type policyReplayDiff struct {
	Changed         int                  `json:"changed"`
	AllowedToDenied int                  `json:"allowed_to_denied"`
	DeniedToAllowed int                  `json:"denied_to_allowed"`
	Changes         []policyReplayChange `json:"changes"`
}

type policyReplayResponse struct {
	policyReplaySummary
	Baseline *policyReplaySummary `json:"baseline,omitempty"`
	Diff     *policyReplayDiff    `json:"diff,omitempty"`
}

// ReplayPolicies compiles candidate and optional baseline policies on the server.
// File-backed policies are bare filenames under the configured policy directory;
// inline bodies are parsed without filesystem access. No arbitrary paths or
// client-provided baseline decisions are accepted.
func (h *Handler) ReplayPolicies(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReplayBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req policyReplayRequest
	if err := decoder.Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = errors.New("expected one JSON object")
		}
		writeDecodeError(w, err)
		return
	}
	if len(req.Policies) == 0 {
		http.Error(w, "policies is required", http.StatusBadRequest)
		return
	}
	if len(req.Requests) == 0 || len(req.Requests) > maxReplayItems {
		http.Error(w, "requests must contain between 1 and 1000 items", http.StatusBadRequest)
		return
	}
	if len(req.Policies)+len(req.BaselinePolicies) > maxReplayItems {
		http.Error(w, "at most 1000 policy bodies may be supplied", http.StatusBadRequest)
		return
	}

	remaining := maxReplayBytes
	engine, err := h.loadEngine(req.Policies, &remaining)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var baseline *policy.CELEngine
	if len(req.BaselinePolicies) > 0 {
		baseline, err = h.loadEngine(req.BaselinePolicies, &remaining)
		if err != nil {
			http.Error(w, "baseline: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	response := policyReplayResponse{policyReplaySummary: replay(r, engine, req.Requests)}
	if baseline != nil {
		summary := replay(r, baseline, req.Requests)
		response.Baseline = &summary
		response.Diff = diffReplay(summary, response.policyReplaySummary)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "policy replay body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
}

func (h *Handler) loadEngine(sources []policySource, remaining *int) (*policy.CELEngine, error) {
	var root *os.Root
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()
	rules := make([]policy.Rule, 0, len(sources))
	for _, source := range sources {
		if err := validatePolicyName(source.Name, source.Body == ""); err != nil {
			return nil, err
		}
		data := []byte(source.Body)
		if source.Body == "" {
			if root == nil {
				var err error
				root, err = os.OpenRoot(h.policyDir)
				if err != nil {
					return nil, errors.New("cannot open configured policy directory")
				}
			}
			file, err := root.Open(source.Name)
			if err != nil {
				return nil, fmt.Errorf("cannot load policy %q from configured directory", source.Name)
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				_ = file.Close()
				return nil, fmt.Errorf("policy %q is not a regular file", source.Name)
			}
			data, err = io.ReadAll(io.LimitReader(file, int64(*remaining)+1))
			_ = file.Close()
			if err != nil {
				return nil, fmt.Errorf("cannot read policy %q", source.Name)
			}
		}
		if len(data) > *remaining {
			return nil, errors.New("combined policy bodies exceed 1 MiB")
		}
		*remaining -= len(data)
		rule, err := policy.RuleFromBytes(source.Name, data)
		if err != nil {
			return nil, err
		}
		if source.Body != "" {
			rule.Source = "inline:" + source.Name
		}
		rules = append(rules, rule)
	}
	return policy.NewEngine(rules...)
}

func validatePolicyName(name string, fileBacked bool) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || strings.ContainsAny(name, "/\\\x00:") {
		return errors.New("policy name must be a bare filename, not a path")
	}
	if fileBacked {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".cel", ".yaml", ".yml":
		default:
			return errors.New("named policies must have a .cel, .yaml, or .yml extension")
		}
	}
	return nil
}

func replay(r *http.Request, engine *policy.CELEngine, requests []model.ToolRequest) policyReplaySummary {
	summary := policyReplaySummary{Total: len(requests), Results: make([]policyReplayResult, 0, len(requests))}
	for index, request := range requests {
		decision, err := engine.Evaluate(r.Context(), request)
		result := policyReplayResult{Index: index, Request: request, Decision: decision}
		if err != nil {
			summary.Errors++
			result.Error = err.Error()
		}
		if decision.Allowed {
			summary.Allowed++
		} else {
			summary.Denied++
		}
		summary.Results = append(summary.Results, result)
	}
	return summary
}

func diffReplay(before, after policyReplaySummary) *policyReplayDiff {
	diff := &policyReplayDiff{Changes: []policyReplayChange{}}
	for index, candidate := range after.Results {
		baseline := before.Results[index]
		if baseline.Decision.Allowed == candidate.Decision.Allowed && baseline.Decision.Rule == candidate.Decision.Rule &&
			baseline.Decision.Reason == candidate.Decision.Reason && baseline.Error == candidate.Error {
			continue
		}
		diff.Changed++
		if baseline.Decision.Allowed && !candidate.Decision.Allowed {
			diff.AllowedToDenied++
		}
		if !baseline.Decision.Allowed && candidate.Decision.Allowed {
			diff.DeniedToAllowed++
		}
		diff.Changes = append(diff.Changes, policyReplayChange{
			Index: index, Before: baseline.Decision, After: candidate.Decision,
			BeforeError: baseline.Error, AfterError: candidate.Error,
		})
	}
	return diff
}
