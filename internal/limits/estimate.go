package limits

import (
	"encoding/json"

	"github.com/omniswitch-dev/omniswitch/internal/provider"
)

// EstimateChat is an approximate pre-check, not a tokenizer: about one token
// per four bytes of prompt text plus three per message. Reserve max_tokens (or
// max_completion_tokens) for output when set; otherwise reserve 256 tokens.
func EstimateChat(request provider.ChatRequest) Usage {
	input := int64(0)
	for _, message := range request.Messages {
		text := message.Content
		for _, call := range message.ToolCalls {
			text += call.Function.Name + call.Function.Arguments
		}
		input += textTokens(text) + 3
	}
	for _, tool := range request.Tools {
		parameters, _ := json.Marshal(tool.Function.Parameters)
		input += textTokens(tool.Function.Name + tool.Function.Description + string(parameters))
	}
	output := int64(256)
	if request.MaxTokens != nil {
		output = int64(max(0, *request.MaxTokens))
	}
	if request.MaxCompletionTokens != nil {
		output = int64(max(0, *request.MaxCompletionTokens))
	}
	return Usage{InputTokens: input, OutputTokens: output}
}

func textTokens(text string) int64 { return int64((len(text) + 3) / 4) }

func EstimateEmbedding(request provider.EmbeddingRequest) Usage {
	input, _ := json.Marshal(request.Input)
	var text string
	if json.Unmarshal(input, &text) == nil {
		return Usage{InputTokens: textTokens(text)}
	}
	var texts []string
	if json.Unmarshal(input, &texts) == nil {
		var count int64
		for _, value := range texts {
			count += textTokens(value)
		}
		return Usage{InputTokens: count}
	}
	var tokens []int
	if json.Unmarshal(input, &tokens) == nil {
		return Usage{InputTokens: int64(len(tokens))}
	}
	var batches [][]int
	if json.Unmarshal(input, &batches) == nil {
		var count int64
		for _, batch := range batches {
			count += int64(len(batch))
		}
		return Usage{InputTokens: count}
	}
	return Usage{InputTokens: textTokens(string(input))}
}

func EstimateRerank(request provider.RerankRequest) Usage {
	input := textTokens(request.Query)
	for _, document := range request.Documents {
		text, ok := document.(string)
		if !ok {
			if fields, ok := document.(map[string]any); ok {
				text, _ = fields["text"].(string)
				if text == "" {
					text, _ = fields["content"].(string)
				}
			}
			if text == "" {
				encoded, _ := json.Marshal(document)
				text = string(encoded)
			}
		}
		input += textTokens(text)
	}
	return Usage{InputTokens: input}
}

func FromProvider(usage provider.Usage, costUSD float64) Usage {
	return Usage{InputTokens: int64(usage.PromptTokens), OutputTokens: int64(usage.CompletionTokens), TotalTokens: int64(usage.TotalTokens), USD: costUSD}
}
