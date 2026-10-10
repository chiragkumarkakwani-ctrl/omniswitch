package provider

import "encoding/json"

// Usage's normalized fields are also the hook used by compatibility adapters:
// CachedPromptTokens, CacheWriteTokens, and ReasoningTokens. OpenAI-compatible
// wire formats keep the standard nested token-details objects.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	var decoded struct {
		plain
		PromptDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		CachedPromptTokens int `json:"cached_prompt_tokens"`
		ReasoningTokens    int `json:"reasoning_tokens"`
		PromptCacheHit     int `json:"prompt_cache_hit_tokens"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = Usage(decoded.plain)
	u.Known = knownUsageFields(data, "prompt_tokens", "completion_tokens", "total_tokens")
	u.CachedPromptTokens = max(decoded.CachedPromptTokens, decoded.PromptCacheHit)
	u.ReasoningTokens = decoded.ReasoningTokens
	if decoded.PromptDetails != nil {
		u.CachedPromptTokens = decoded.PromptDetails.CachedTokens
	}
	if decoded.CompletionDetails != nil {
		u.ReasoningTokens = decoded.CompletionDetails.ReasoningTokens
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return nil
}

func (u Usage) MarshalJSON() ([]byte, error) {
	type plain Usage
	var promptDetails, completionDetails map[string]int
	if u.CachedPromptTokens != 0 {
		promptDetails = map[string]int{"cached_tokens": u.CachedPromptTokens}
	}
	if u.ReasoningTokens != 0 {
		completionDetails = map[string]int{"reasoning_tokens": u.ReasoningTokens}
	}
	return json.Marshal(struct {
		plain
		PromptDetails     map[string]int `json:"prompt_tokens_details,omitempty"`
		CompletionDetails map[string]int `json:"completion_tokens_details,omitempty"`
	}{plain: plain(u), PromptDetails: promptDetails, CompletionDetails: completionDetails})
}

func knownUsageFields(data []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for _, name := range names {
		if value, found := fields[name]; found && string(value) != "null" {
			return true
		}
	}
	return false
}

// Anthropic reports uncached input separately from reads and cache creation.
type anthropicUsage struct {
	Known                    bool `json:"-"`
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
}

func (u *anthropicUsage) UnmarshalJSON(data []byte) error {
	type plain anthropicUsage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = anthropicUsage(decoded)
	u.Known = knownUsageFields(data, "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
	return nil
}

func (u anthropicUsage) normalized() Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return Usage{Known: u.Known || prompt != 0 || u.OutputTokens != 0, PromptTokens: prompt, CompletionTokens: u.OutputTokens, TotalTokens: prompt + u.OutputTokens, CachedPromptTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
}

type geminiUsage struct {
	Known                   bool `json:"-"`
	PromptTokenCount        int  `json:"promptTokenCount"`
	CandidatesTokenCount    int  `json:"candidatesTokenCount"`
	TotalTokenCount         int  `json:"totalTokenCount"`
	CachedContentTokenCount int  `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int  `json:"thoughtsTokenCount"`
}

func (u *geminiUsage) UnmarshalJSON(data []byte) error {
	type plain geminiUsage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = geminiUsage(decoded)
	u.Known = knownUsageFields(data, "promptTokenCount", "candidatesTokenCount", "totalTokenCount", "thoughtsTokenCount")
	return nil
}

func (u geminiUsage) normalized() Usage {
	output := u.CandidatesTokenCount + u.ThoughtsTokenCount
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + output
	}
	return Usage{Known: u.Known || total != 0, PromptTokens: u.PromptTokenCount, CompletionTokens: output, TotalTokens: total, CachedPromptTokens: u.CachedContentTokenCount, ReasoningTokens: u.ThoughtsTokenCount}
}

// Converse inputTokens/totalTokens exclude cache reads and writes:
// https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
// Normalize them into totals that include the cache subdivisions once.
type bedrockUsage struct {
	Known                 bool `json:"-"`
	InputTokens           int  `json:"inputTokens"`
	OutputTokens          int  `json:"outputTokens"`
	TotalTokens           int  `json:"totalTokens"`
	CacheReadInputTokens  int  `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int  `json:"cacheWriteInputTokens"`
}

func (u *bedrockUsage) UnmarshalJSON(data []byte) error {
	type plain bedrockUsage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = bedrockUsage(decoded)
	u.Known = knownUsageFields(data, "inputTokens", "outputTokens", "totalTokens", "cacheReadInputTokens", "cacheWriteInputTokens")
	return nil
}

func (u bedrockUsage) normalized() Usage {
	cached := u.CacheReadInputTokens + u.CacheWriteInputTokens
	prompt := u.InputTokens + cached
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	total += cached
	return Usage{Known: u.Known || total != 0, PromptTokens: prompt, CompletionTokens: u.OutputTokens, TotalTokens: total, CachedPromptTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheWriteInputTokens}
}
