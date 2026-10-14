// Package main is a throwaway self-test for the cpakit layer: it proves the
// generated C ABI glue, the official SDK types, registration, model publication,
// and executor dispatch all work against a real host. It is removed once the
// provider plugins are ported.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/sivan/cpa-plugins/internal/cpakit"
)

const providerKey = "cpa-smoke"

type gateway struct{}

func init() { cpakit.RegisterPlugin(gateway{}) }

func (gateway) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             "CPA Smoke Test",
		Version:          "0.0.1",
		Author:           "cpa-plugins",
		GitHubRepository: "https://github.com/Sivan757/cpa-plugins",
	}
}

func (gateway) ProviderKey() string { return providerKey }

func (gateway) Capabilities() cpakit.Capabilities {
	return cpakit.Capabilities{
		ModelProvider:         true,
		AuthProvider:          true,
		Executor:              true,
		ExecutorModelScope:    cpakit.ScopeStatic,
		ExecutorInputFormats:  []string{"chat-completions"},
		ExecutorOutputFormats: []string{"chat-completions"},
	}
}

func (gateway) Configure([]byte) error { return nil }

func (gateway) Dispatch(ctx context.Context, method string, request json.RawMessage) (any, error) {
	switch method {
	case "model.static":
		return pluginapi.ModelResponse{Provider: providerKey, Models: []pluginapi.ModelInfo{{
			ID: "cpa-smoke-echo", Object: "model", OwnedBy: providerKey,
			DisplayName: "Smoke Echo", ContextLength: 8192, MaxCompletionTokens: 1024,
			SupportedInputModalities: []string{"text"}, SupportedOutputModalities: []string{"text"},
		}}}, nil
	case "auth.parse":
		var req pluginapi.AuthParseRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, cpakit.NewError("invalid_request", errUnmarshal.Error())
		}
		var document map[string]any
		_ = json.Unmarshal(req.RawJSON, &document)
		if typed, _ := document["type"].(string); typed != providerKey {
			return pluginapi.AuthParseResponse{Handled: false}, nil
		}
		return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
			Provider: providerKey, ID: req.FileName, FileName: req.FileName,
			StorageJSON: req.RawJSON,
			Attributes:  map[string]string{"key": "smoke"},
		}}, nil
	case "executor.execute", "executor.execute_stream":
		var req pluginapi.ExecutorRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, cpakit.NewError("invalid_request", errUnmarshal.Error())
		}
		body, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-smoke",
			"object":  "chat.completion",
			"created": 0,
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "smoke ok"},
				"finish_reason": "stop",
			}},
		})
		return pluginapi.ExecutorResponse{
			Payload: body,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}
	return nil, cpakit.ErrUnhandled
}
