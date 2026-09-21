package service

import "strings"

// IsImageOnlyModel reports whether a model can only be served through an image
// endpoint. The gpt-image family keeps growing (1, 1.5, 2, 2.5, 2.5-flare, …), so
// the check matches the family prefix instead of an allow-list that silently falls
// behind: a model the list misses reaches a text-only path, where the host's
// model-execute callback refuses it and the native /v1/images route is bypassed.
func IsImageOnlyModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(model, "gpt-image-") {
		return true
	}
	return strings.Contains(model, "imagine-image") || strings.Contains(model, "imagine-video")
}
