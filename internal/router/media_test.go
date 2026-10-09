package router

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

func TestMediaRoutingCapabilityAndFallback(t *testing.T) {
	endpoints := []struct {
		path string
		call func(*Router) (provider.ProviderMeta, error)
	}{
		{"/v1/images/generations", func(r *Router) (provider.ProviderMeta, error) {
			_, meta, err := r.ImageGeneration(context.Background(), provider.ImageRequest{Model: "media-model"}, "")
			return meta, err
		}},
		{"/v1/audio/transcriptions", func(r *Router) (provider.ProviderMeta, error) {
			_, meta, err := r.Transcription(context.Background(), provider.TranscriptionRequest{Model: "media-model"}, "")
			return meta, err
		}},
		{"/v1/audio/speech", func(r *Router) (provider.ProviderMeta, error) {
			body, _, meta, err := r.Speech(context.Background(), provider.SpeechRequest{Model: "media-model"}, "")
			if body != nil {
				_ = body.Close()
			}
			return meta, err
		}},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.path, func(t *testing.T) {
			t.Run("capable_alias_fallback", func(t *testing.T) {
				registry := provider.NewRegistry()
				registry.Register(&fakeProvider{name: "chat", model: "media-model"})
				alias := provider.NewAlias("team", &mediaRoutingProvider{fakeProvider: fakeProvider{name: "native", model: "media-model"}})
				registry.Register(alias)
				rtr := New(registry)
				rtr.SetRoute("media-model", Route{Provider: "chat", Fallbacks: []string{alias.Name()}})
				meta, err := endpoint.call(rtr)
				if err != nil || !meta.Fallback || meta.Provider != alias.Name() || meta.Model != "media-model" {
					t.Fatalf("fallback meta/error = %+v/%v", meta, err)
				}
			})
			t.Run("capability_errors_do_not_open_circuits", func(t *testing.T) {
				registry := provider.NewRegistry()
				unsupported := provider.NewAlias("team", &fakeProvider{name: "chat", model: "media-model"})
				registry.Register(unsupported)
				rtr := New(registry)
				breaker := NewCircuitBreaker(1, time.Hour)
				rtr.SetCircuitBreaker(breaker)
				rtr.SetRoute("media-model", Route{Provider: unsupported.Name()})
				for attempt := 0; attempt < 2; attempt++ {
					_, err := endpoint.call(rtr)
					var capability *provider.UnsupportedEndpointError
					if !errors.As(err, &capability) || capability.Endpoint != endpoint.path {
						t.Fatalf("capability error = %v, want endpoint %s", err, endpoint.path)
					}
				}
				if state := breaker.State(unsupported.Name()); state != CircuitClosed {
					t.Fatalf("capability error opened circuit: %s", state)
				}
			})
			t.Run("upstream_error_survives_unsupported_fallback", func(t *testing.T) {
				upstreamErr := errors.New("media upstream failed")
				registry := provider.NewRegistry()
				registry.Register(&mediaRoutingProvider{fakeProvider: fakeProvider{name: "native", model: "media-model", err: upstreamErr}})
				registry.Register(&fakeProvider{name: "chat", model: "media-model"})
				unsupported := provider.NewAlias("team", &fakeProvider{name: "chat", model: "media-model"})
				registry.Register(unsupported)
				rtr := New(registry)
				rtr.SetRoute("media-model", Route{Provider: "native", Fallbacks: []string{"chat", unsupported.Name()}})
				_, err := endpoint.call(rtr)
				var capability *provider.UnsupportedEndpointError
				if !errors.Is(err, upstreamErr) || errors.As(err, &capability) {
					t.Fatalf("mixed fallback error = %v, want original upstream failure", err)
				}
			})
		})
	}
}

type mediaRoutingProvider struct{ fakeProvider }

func (p *mediaRoutingProvider) ImageGeneration(context.Context, provider.ImageRequest) (provider.ImageResponse, provider.ProviderMeta, error) {
	return provider.ImageResponse{}, provider.ProviderMeta{Provider: p.Name()}, p.err
}

func (p *mediaRoutingProvider) Transcription(context.Context, provider.TranscriptionRequest) (provider.TranscriptionResponse, provider.ProviderMeta, error) {
	return provider.TranscriptionResponse{Text: "transcript"}, provider.ProviderMeta{Provider: p.Name()}, p.err
}

func (p *mediaRoutingProvider) Speech(context.Context, provider.SpeechRequest) (io.ReadCloser, string, provider.ProviderMeta, error) {
	if p.err != nil {
		return nil, "", provider.ProviderMeta{Provider: p.Name()}, p.err
	}
	return io.NopCloser(strings.NewReader("audio")), "audio/mpeg", provider.ProviderMeta{Provider: p.Name()}, nil
}
