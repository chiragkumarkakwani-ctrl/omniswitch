package gateway

import (
	"context"
	"fmt"

	"github.com/omniswitch-dev/omniswitch/internal/limits"
	"github.com/omniswitch-dev/omniswitch/internal/provider/catalog"
)

type catalogModelResponse struct {
	Provider              string             `json:"provider"`
	ID                    string             `json:"id"`
	InputPerMillion       float64            `json:"input_per_million"`
	OutputPerMillion      float64            `json:"output_per_million"`
	CachedInputPerMillion float64            `json:"cached_input_per_million"`
	PerImage              map[string]float64 `json:"per_image"`
	PerAudioMinute        float64            `json:"per_audio_minute"`
	PerMillionCharacters  float64            `json:"per_million_characters"`
	SourceURL             string             `json:"source_url"`
	Source                string             `json:"source"`
	VerifiedOn            string             `json:"verified_on"`
}

type catalogResponseBody struct {
	VerifiedOn string                 `json:"verified_on"`
	Models     []catalogModelResponse `json:"models"`
}

func catalogResponse(provider string) catalogResponseBody {
	effective := catalog.Active()
	entries := effective.All()
	if provider != "" {
		entries = effective.ForProvider(provider)
	}
	response := catalogResponseBody{VerifiedOn: effective.VerifiedOn(), Models: make([]catalogModelResponse, 0, len(entries))}
	for _, model := range entries {
		cachedPrice := model.InputPerMillion
		if model.CachedInputPerMillion != nil {
			cachedPrice = *model.CachedInputPerMillion
		}
		images := model.PerImage
		if images == nil {
			images = map[string]float64{}
		}
		response.Models = append(response.Models, catalogModelResponse{
			Provider: model.Provider, ID: model.ID, InputPerMillion: model.InputPerMillion, OutputPerMillion: model.OutputPerMillion,
			CachedInputPerMillion: cachedPrice, PerImage: images, PerAudioMinute: model.PerAudioMinute,
			PerMillionCharacters: model.PerMillionCharacters, SourceURL: model.SourceURL, Source: model.Source, VerifiedOn: model.VerifiedOn,
		})
	}
	return response
}

func (h *Handler) limitIdentityForKey(ctx context.Context, id string) (limits.Identity, error) {
	if h.store == nil {
		return limits.Identity{}, fmt.Errorf("store is unavailable")
	}
	key, err := h.store.GetAPIKeyByID(ctx, id)
	if err != nil {
		return limits.Identity{}, err
	}
	identity := limits.Identity{KeyID: key.ID, WorkspaceID: key.WorkspaceID, RateLimit: key.RateLimit,
		RequestsPerMinute: key.RequestsPerMinute, TokensPerMinute: key.TokensPerMinute, USDPerDay: key.USDPerDay}
	if key.WorkspaceID != "" {
		workspace, err := h.store.GetWorkspaceByID(ctx, key.WorkspaceID)
		if err != nil {
			return limits.Identity{}, err
		}
		identity.OrganizationID = workspace.OrganizationID
	}
	return identity, nil
}
