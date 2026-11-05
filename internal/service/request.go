package service

import (
	"encoding/json"
	"strings"
)

func estimateTokens(body []byte, defaultOutput, maxTotal int64) (input, output int64) {
	output = defaultOutput
	if body == nil {
		input = 1
	} else {
		// Conservative character-based upper bound: 1 token per rune-ish byte pair, floor 1.
		input = int64(len(body)/2) + 1
	}
	// Prefer explicit protocol limits when present.
	if explicit := extractOutputLimit(body); explicit > 0 {
		output = explicit
	}
	if input > maxTotal {
		input = maxTotal
	}
	if output > maxTotal {
		output = maxTotal
	}
	if input+output > maxTotal {
		// Keep output preference and shrink input.
		if output >= maxTotal {
			output = maxTotal
			input = 0
		} else {
			input = maxTotal - output
		}
	}
	if input <= 0 {
		input = 1
	}
	if output <= 0 {
		output = 1
	}
	return input, output
}

// requestFields holds the few top-level members the reserve plan reads. Decoding
// into it skips the rest (the multi-MB input history) instead of building a map.
// Fields stay `any` so a mistyped member is ignored like the old map lookup did.
type requestFields struct {
	Model               any `json:"model"`
	ServiceTier         any `json:"service_tier"`
	Tier                any `json:"tier"`
	MaxTokens           any `json:"max_tokens"`
	MaxOutputTokens     any `json:"max_output_tokens"`
	MaxCompletionTokens any `json:"max_completion_tokens"`
	GenerationConfig    any `json:"generationConfig"`
	N                   any `json:"n"`
	NImages             any `json:"n_images"`
	NumImages           any `json:"num_images"`
	ImageCount          any `json:"image_count"`
}

func decodeRequestFields(body []byte) (requestFields, bool) {
	var root requestFields
	if err := json.Unmarshal(body, &root); err != nil {
		return root, false
	}
	return root, true
}

func extractModel(body []byte) string {
	root, ok := decodeRequestFields(body)
	if !ok {
		return ""
	}
	return firstTrimmedString(root.Model)
}

func extractServiceTier(body []byte) string {
	root, ok := decodeRequestFields(body)
	if !ok {
		return ""
	}
	return firstTrimmedString(root.ServiceTier, root.Tier)
}

func firstTrimmedString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func extractOutputLimit(body []byte) int64 {
	root, ok := decodeRequestFields(body)
	if !ok {
		return 0
	}
	for _, value := range []any{root.MaxTokens, root.MaxOutputTokens, root.MaxCompletionTokens} {
		if typed, ok := value.(float64); ok && typed > 0 {
			return int64(typed)
		}
	}
	if gen, ok := root.GenerationConfig.(map[string]any); ok {
		if value, ok := gen["maxOutputTokens"].(float64); ok && value > 0 {
			return int64(value)
		}
	}
	return 0
}

func extractImageCount(body []byte) int64 {
	const maxImages int64 = 32
	if len(body) == 0 {
		return 1
	}
	root, ok := decodeRequestFields(body)
	if !ok {
		return 1
	}
	for _, value := range []any{root.N, root.NImages, root.NumImages, root.ImageCount} {
		if count := positiveInt64(value); count > 0 {
			if count > maxImages {
				return maxImages
			}
			return count
		}
	}
	return 1
}

func positiveInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return int64(typed)
		}
	case int64:
		if typed > 0 {
			return typed
		}
	case int:
		if typed > 0 {
			return int64(typed)
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}
