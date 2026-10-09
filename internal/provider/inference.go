package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type InferenceReplica struct {
	URL      string
	Runtime  string
	Models   []string
	Priority int
}

// InferenceReplicaHealth is a snapshot of a replica's latest health probe.
// Healthy and LastCheck are nil until the first probe completes.
type InferenceReplicaHealth struct {
	URL       string     `json:"url"`
	Runtime   string     `json:"runtime"`
	Models    []string   `json:"models"`
	Priority  int        `json:"priority"`
	Healthy   *bool      `json:"healthy"`
	LastCheck *time.Time `json:"last_check"`
	LatencyMS float64    `json:"latency_ms"`
	QueueLen  int        `json:"queue_len"`
}

type InferenceBackend struct {
	name     string
	replicas []InferenceReplica
	client   *http.Client
	health   *healthTracker
	cancel   context.CancelFunc
	done     chan struct{}
}

type healthTracker struct {
	mu        sync.Mutex
	latency   map[string]time.Duration
	queueLen  map[string]int
	lastCheck map[string]time.Time
	healthy   map[string]bool
}

func NewInferenceBackend(name string, replicas []InferenceReplica, healthInterval time.Duration) *InferenceBackend {
	if healthInterval <= 0 {
		healthInterval = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	backend := &InferenceBackend{
		name:     strings.TrimSpace(name),
		replicas: append([]InferenceReplica(nil), replicas...),
		client:   &http.Client{Timeout: 120 * time.Second},
		cancel:   cancel,
		done:     make(chan struct{}),
		health: &healthTracker{
			latency:   map[string]time.Duration{},
			queueLen:  map[string]int{},
			lastCheck: map[string]time.Time{},
			healthy:   map[string]bool{},
		},
	}
	if backend.name == "" {
		backend.name = "inference"
	}
	for i := range backend.replicas {
		backend.replicas[i].Models = append([]string(nil), backend.replicas[i].Models...)
	}
	go backend.probeLoop(ctx, healthInterval)
	return backend
}

func (b *InferenceBackend) Name() string { return b.name }

// Close stops health probes and waits for in-flight probes to exit.
func (b *InferenceBackend) Close() error {
	b.cancel()
	<-b.done
	return nil
}

// ReplicaHealth returns independent snapshots in configuration order. Admin
// handlers can call this without issuing network requests or sharing tracker maps.
func (b *InferenceBackend) ReplicaHealth() []InferenceReplicaHealth {
	b.health.mu.Lock()
	defer b.health.mu.Unlock()
	snapshots := make([]InferenceReplicaHealth, 0, len(b.replicas))
	for _, replica := range b.replicas {
		runtime := strings.ToLower(strings.TrimSpace(replica.Runtime))
		if runtime == "" {
			runtime = "openai"
		}
		snapshot := InferenceReplicaHealth{
			URL: replica.URL, Runtime: runtime,
			Models: append([]string(nil), replica.Models...), Priority: replica.Priority,
			LatencyMS: float64(b.health.latency[replica.URL]) / float64(time.Millisecond),
			QueueLen:  b.health.queueLen[replica.URL],
		}
		if checked, ok := b.health.lastCheck[replica.URL]; ok {
			healthy := b.health.healthy[replica.URL]
			snapshot.Healthy = &healthy
			snapshot.LastCheck = &checked
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func (b *InferenceBackend) Models() []ModelInfo {
	seen := map[string]bool{}
	var models []ModelInfo
	for _, replica := range b.replicas {
		for _, model := range replica.Models {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			models = append(models, ModelInfo{ID: model, Object: "model", OwnedBy: b.name, Provider: b.name})
		}
	}
	return models
}

func (b *InferenceBackend) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, ProviderMeta, error) {
	start := time.Now()
	meta := ProviderMeta{Provider: b.name, ProviderType: "inference", Model: req.Model, Timestamp: start}
	replica, ok := b.selectReplica(req.Model)
	if !ok {
		return ChatResponse{}, meta, fmt.Errorf("no inference replica supports model %q", req.Model)
	}
	var resp ChatResponse
	var err error
	switch normalizedRuntime(replica) {
	case "tgi":
		resp, err = b.chatTGI(ctx, replica, req)
	case "triton":
		err = fmt.Errorf("triton runtime requires a model-specific adapter; use an OpenAI-compatible vLLM endpoint for chat completions")
	default:
		resp, err = b.chatOpenAICompatible(ctx, replica, req)
	}
	meta.Latency = time.Since(start)
	b.health.recordLatency(replica.URL, meta.Latency)
	if err != nil {
		meta.Error = err.Error()
		return ChatResponse{}, meta, err
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	return resp, meta, nil
}

func (b *InferenceBackend) ChatCompletionStream(ctx context.Context, req ChatRequest) (<-chan ChatResponseChunk, ProviderMeta, error) {
	start := time.Now()
	meta := ProviderMeta{Provider: b.name, ProviderType: "inference", Model: req.Model, Timestamp: start}
	replica, ok := b.selectReplica(req.Model)
	if !ok {
		return nil, meta, fmt.Errorf("no inference replica supports model %q", req.Model)
	}
	if normalizedRuntime(replica) != "openai" {
		resp, _, err := b.ChatCompletion(ctx, req)
		if err != nil {
			meta.Error = err.Error()
			meta.Latency = time.Since(start)
			return nil, meta, err
		}
		meta.Latency = time.Since(start)
		return StreamFromResponse(ctx, resp), meta, nil
	}
	chunks, meta, err := streamOpenAICompatible(ctx, b.client, strings.TrimRight(replica.URL, "/")+"/chat/completions", "", req, b.name, nil)
	meta.ProviderType = "inference"
	if err == nil {
		b.health.recordLatency(replica.URL, meta.Latency)
	}
	return chunks, meta, err
}

func (b *InferenceBackend) selectReplica(model string) (InferenceReplica, bool) {
	var candidates []InferenceReplica
	for _, replica := range b.replicas {
		if len(replica.Models) == 0 || containsModel(replica.Models, model) {
			candidates = append(candidates, replica)
		}
	}
	if len(candidates) == 0 {
		return InferenceReplica{}, false
	}
	b.health.mu.Lock()
	defer b.health.mu.Unlock()
	sort.SliceStable(candidates, func(i, j int) bool {
		leftHealthy, leftChecked := b.health.healthy[candidates[i].URL]
		rightHealthy, rightChecked := b.health.healthy[candidates[j].URL]
		leftUnhealthy := leftChecked && !leftHealthy
		rightUnhealthy := rightChecked && !rightHealthy
		if leftUnhealthy != rightUnhealthy {
			return !leftUnhealthy
		}
		return b.health.scoreLocked(candidates[i]) < b.health.scoreLocked(candidates[j])
	})
	return candidates[0], true
}

func (b *InferenceBackend) chatOpenAICompatible(ctx context.Context, replica InferenceReplica, req ChatRequest) (ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(replica.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, fmt.Errorf("inference replica %s error (status %d): %s", replica.URL, resp.StatusCode, string(payload))
	}
	var out ChatResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return ChatResponse{}, err
	}
	return out, nil
}

func (b *InferenceBackend) chatTGI(ctx context.Context, replica InferenceReplica, req ChatRequest) (ChatResponse, error) {
	prompt := chatPrompt(req.Messages)
	body, err := json.Marshal(map[string]any{
		"inputs": prompt,
		"parameters": map[string]any{
			"max_new_tokens": optionalInt(req.MaxTokens, 512),
			"temperature":    optionalFloat(req.Temperature, 0.7),
			"top_p":          optionalFloat(req.TopP, 1),
		},
	})
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(replica.URL, "/")+"/generate", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, fmt.Errorf("inference TGI replica %s error (status %d): %s", replica.URL, resp.StatusCode, string(payload))
	}
	var tgi struct {
		GeneratedText string `json:"generated_text"`
	}
	if err := json.Unmarshal(payload, &tgi); err != nil {
		return ChatResponse{}, err
	}
	return ChatResponse{
		ID:      fmt.Sprintf("inf_%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: tgi.GeneratedText}, FinishReason: "stop"}},
	}, nil
}

func (b *InferenceBackend) probeLoop(ctx context.Context, interval time.Duration) {
	defer close(b.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b.probeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *InferenceBackend) probeOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, replica := range b.replicas {
		wg.Add(1)
		go func(replica InferenceReplica) {
			defer wg.Done()
			start := time.Now()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, inferenceHealthURL(replica), nil)
			if err != nil {
				b.health.recordProbe(replica.URL, time.Since(start), 0, false)
				return
			}
			resp, err := b.client.Do(req)
			if err != nil {
				b.health.recordProbe(replica.URL, time.Since(start), 0, false)
				return
			}
			defer resp.Body.Close()
			var status struct {
				QueueLen int `json:"queue_len"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status)
			healthy := resp.StatusCode >= 200 && resp.StatusCode < 300
			b.health.recordProbe(replica.URL, time.Since(start), maxInt(status.QueueLen, 0), healthy)
		}(replica)
	}
	wg.Wait()
}

func inferenceHealthURL(replica InferenceReplica) string {
	base := strings.TrimSuffix(strings.TrimRight(replica.URL, "/"), "/v1")
	if strings.EqualFold(strings.TrimSpace(replica.Runtime), "ollama") {
		return base + "/api/tags"
	}
	return base + "/health"
}

func (t *healthTracker) recordLatency(url string, latency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latency[url] = latency
}

func (t *healthTracker) recordProbe(url string, latency time.Duration, queueLen int, healthy bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latency[url] = latency
	t.queueLen[url] = queueLen
	t.lastCheck[url] = time.Now().UTC()
	t.healthy[url] = healthy
}

// scoreLocked ranks observations without turning health penalties into fake
// measured latency/queue data. The caller holds mu for a consistent sort.
func (t *healthTracker) scoreLocked(replica InferenceReplica) time.Duration {
	latency := t.latency[replica.URL]
	if latency <= 0 {
		latency = 500 * time.Millisecond
	}
	queueLen := t.queueLen[replica.URL]
	priorityPenalty := time.Duration(maxInt(replica.Priority, 0)) * 100 * time.Millisecond
	return latency*time.Duration(1+queueLen/10) + priorityPenalty
}

func normalizedRuntime(replica InferenceReplica) string {
	runtime := strings.ToLower(strings.TrimSpace(replica.Runtime))
	switch runtime {
	case "tgi", "text-generation-inference":
		return "tgi"
	case "triton":
		return "triton"
	default:
		return "openai"
	}
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func chatPrompt(messages []Message) string {
	var parts []string
	for _, message := range messages {
		parts = append(parts, strings.ToUpper(message.Role)+": "+message.Text())
	}
	return strings.Join(parts, "\n") + "\nASSISTANT:"
}

func optionalInt(value *int, fallback int) int {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}

func optionalFloat(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
