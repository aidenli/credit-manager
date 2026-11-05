package service

import "testing"

func TestRequestFieldExtractors(t *testing.T) {
	body := []byte(`{"model":" gpt-x ","tier":"flex","max_output_tokens":2048,"n":64,"input":[{"type":"message","content":"hi"}]}`)
	if got := extractModel(body); got != "gpt-x" {
		t.Fatalf("model = %q", got)
	}
	if got := extractServiceTier(body); got != "flex" {
		t.Fatalf("tier = %q", got)
	}
	if got := extractOutputLimit(body); got != 2048 {
		t.Fatalf("output limit = %d", got)
	}
	if got := extractImageCount(body); got != 32 {
		t.Fatalf("image count = %d", got)
	}
	gemini := []byte(`{"model":7,"service_tier":"priority","max_tokens":"9","generationConfig":{"maxOutputTokens":512}}`)
	if extractModel(gemini) != "" || extractServiceTier(gemini) != "priority" || extractOutputLimit(gemini) != 512 {
		t.Fatal("mistyped members must be ignored like the map lookup did")
	}
	if extractModel([]byte(`not json`)) != "" || extractImageCount([]byte(`not json`)) != 1 {
		t.Fatal("invalid body fallbacks changed")
	}
}
