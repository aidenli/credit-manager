package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

func decodeLifecycle(raw []byte) (lifecycleRequest, error) {
	if len(raw) == 0 {
		return lifecycleRequest{}, nil
	}
	var envelope struct {
		ConfigYAML    json.RawMessage `json:"config_yaml"`
		SchemaVersion uint32          `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return lifecycleRequest{}, fmt.Errorf("decode lifecycle request: %w", err)
	}
	configYAML, err := decodeLifecycleConfigYAML(envelope.ConfigYAML)
	if err != nil {
		return lifecycleRequest{}, err
	}
	return lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: envelope.SchemaVersion}, nil
}

func decodeLifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(text); decodeErr == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, fmt.Errorf("config_yaml must be a base64/plain string or byte array")
}

// Schema 7 only adds opt-in body omission; keep SDK v7.3.4 and advertise no newer contracts.
const supportedRPCSchemaVersion uint32 = 7

func negotiateRPCSchema(hostSchema uint32) uint32 {
	if hostSchema == 0 {
		return 1
	}
	return min(hostSchema, supportedRPCSchemaVersion)
}
