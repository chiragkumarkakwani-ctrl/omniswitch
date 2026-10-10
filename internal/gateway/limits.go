package gateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider"
	"github.com/omniswitch-dev/omniswitch/internal/router"
)

func (h *Handler) SetLimits(enforcer *limits.Enforcer) { h.limiter.Store(enforcer) }
func (h *Handler) Limits() *limits.Enforcer            { return h.limiter.Load() }

// ReserveLimits is the common media/embedding/chat reservation API. Call after
// resolving the provider/model; Commit usage and cost when the response ends.
// Identity must come from LimitIdentity, not client-provided quota values.
func (h *Handler) ReserveLimits(ctx context.Context, identity limits.Identity, model string, estimate limits.Usage) (*limits.Reservation, []limits.Snapshot, error) {
	if enforcer := h.Limits(); enforcer != nil {
		return enforcer.Reserve(ctx, identity, model, estimate)
	}
	return nil, []limits.Snapshot{}, nil
}

type limitState struct {
	handler       *Handler
	writer        http.ResponseWriter
	request       *http.Request
	identity      limits.Identity
	estimate      limits.Usage
	reservation   *limits.Reservation
	resolvedModel string
	requests      []limits.Snapshot
}

func (h *Handler) startLimits(ctx context.Context, w http.ResponseWriter, r *http.Request, model string, estimate limits.Usage) (context.Context, *limitState, error) {
	state := &limitState{handler: h, writer: w, request: r.WithContext(ctx), identity: LimitIdentity(r), estimate: estimate}
	if enforcer := h.Limits(); enforcer != nil {
		check, checked := ctx.Value(requestLimitCheckKey{}).(requestLimitCheck)
		state.requests = requestSnapshots(check.snapshots)
		if !checked {
			snapshots, err := enforcer.CheckRequests(ctx, state.identity, model)
			state.requests = requestSnapshots(snapshots)
			limits.SetHeaders(w.Header(), snapshots)
			if err != nil {
				return ctx, state, err
			}
		} else if !check.modelKnown {
			// Keep request-policy matching on the submitted model even when
			// middleware could not find it in its bounded JSON prefix.
			snapshots, err := enforcer.CheckModelRequests(ctx, state.identity, model)
			state.requests = requestSnapshots(snapshots)
			limits.SetHeaders(w.Header(), snapshots)
			if err != nil {
				return ctx, state, err
			}
		}
	}
	return router.WithPreflight(ctx, state.preflight), state, nil
}

func (s *limitState) preflight(ctx context.Context, current provider.Provider, model string) error {
	s.release()
	target, resolved := provider.PricingTarget(current, model)
	provider.RepriceUsage(&target, resolved, provider.Usage{PromptTokens: int(s.estimate.InputTokens), CompletionTokens: int(s.estimate.OutputTokens)})
	s.resolvedModel = resolved
	estimate := s.estimate
	estimate.USD = target.Cost
	return s.reserve(ctx, resolved, estimate)
}

func (s *limitState) reserve(ctx context.Context, model string, estimate limits.Usage) error {
	reservation, snapshots, err := s.handler.ReserveLimits(ctx, s.identity, model, estimate)
	if s.handler.Limits() != nil {
		s.setHeaders(snapshots)
	}
	s.reservation = reservation
	return err
}

func (s *limitState) cached(ctx context.Context, model string) error {
	resolved := model
	if current, err := s.handler.registry.ResolveModel(model); err == nil {
		_, resolved = provider.PricingTarget(current, model)
	}
	s.resolvedModel = resolved
	estimate := s.estimate
	estimate.USD = 0 // a gateway cache hit makes no billable upstream call
	return s.reserve(ctx, resolved, estimate)
}

func requestSnapshots(snapshots []limits.Snapshot) []limits.Snapshot {
	var requests []limits.Snapshot
	for _, snapshot := range snapshots {
		if snapshot.Unit == limits.Requests {
			requests = append(requests, snapshot)
		}
	}
	return requests
}

func (s *limitState) setHeaders(snapshots []limits.Snapshot) {
	combined := append([]limits.Snapshot(nil), s.requests...)
	for _, snapshot := range snapshots {
		if snapshot.Unit != limits.Requests {
			combined = append(combined, snapshot)
		}
	}
	limits.SetHeaders(s.writer.Header(), combined)
}

func usageKnown(usage provider.Usage) bool {
	return usage.Known || usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.TotalTokens != 0
}

// complete runs before blocking/buffered headers, or at the end of a live
// stream. If the upstream omitted usage, keep the conservative reservation.
func (s *limitState) complete(ctx context.Context, usage provider.Usage, meta *provider.ProviderMeta, cached bool) error {
	if cached {
		meta.Cost, meta.CostSource = 0, "none"
	} else if usageKnown(usage) {
		provider.RepriceUsage(meta, firstNonEmpty(s.resolvedModel, meta.Model), usage)
	} else {
		meta.Cost, meta.CostSource = 0, "none"
	}
	reservation := s.reservation
	s.reservation = nil
	if reservation == nil || !usageKnown(usage) {
		return nil
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	snapshots, err := reservation.Commit(commitCtx, limits.FromProvider(usage, meta.Cost))
	if err == nil && s.handler.Limits() != nil {
		s.setHeaders(snapshots)
	}
	return err
}

func (s *limitState) release() {
	if s == nil || s.reservation == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.request.Context()), 3*time.Second)
	defer cancel()
	if err := s.reservation.Release(ctx); err != nil {
		log.Printf("failed to release rate limit reservation: %v", err)
	}
	s.reservation = nil
}

func (s *limitState) failure(err error) bool {
	var rejection *limits.Rejection
	var backend *limits.BackendError
	if !errors.As(err, &rejection) && !errors.As(err, &backend) {
		return false
	}
	ctx := context.WithValue(s.request.Context(), resolvedLimitModelKey{}, s.resolvedModel)
	return writeLimitFailure(s.writer, s.request.WithContext(ctx), s.handler.store, err)
}

// Catalog handles the viewer-readable effective pricing catalog.
func (h *Handler) Catalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET is supported")
		return
	}
	writeJSON(w, http.StatusOK, catalogResponse(r.URL.Query().Get("provider")))
}

// LimitsUsage returns the live enforcer snapshot, not request-log aggregates.
// Members/viewers may only inspect their own key, including when key_id is
// explicit. Admins may select any key; without key_id everyone gets their own.
func (h *Handler) LimitsUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET is supported")
		return
	}
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authenticated identity is required")
		return
	}
	keyID := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if keyID == "" {
		keyID = identity.APIKeyID
	}
	if keyID != identity.APIKeyID && roleRank(identity.Role) < roleRank("admin") {
		writeError(w, http.StatusForbidden, "only admins may inspect another key's limit usage")
		return
	}
	limitIdentity := LimitIdentity(r)
	if keyID != identity.APIKeyID {
		var err error
		limitIdentity, err = h.limitIdentityForKey(r.Context(), keyID)
		if err != nil {
			writeError(w, http.StatusNotFound, "API key not found")
			return
		}
	}
	snapshots := []limits.Snapshot{}
	if enforcer := h.Limits(); enforcer != nil {
		var err error
		snapshots, err = enforcer.Snapshot(r.Context(), limitIdentity, r.URL.Query().Get("model"))
		if writeLimitFailure(w, r, h.store, err) {
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": snapshots})
}
