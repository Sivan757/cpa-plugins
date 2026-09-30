package main

import (
	"fmt"
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

func TestZZParseAuthMetadata(t *testing.T) {
	g := newGateway()
	resp, errParse := g.ParseAuth(t.Context(), pluginkit.AuthParseRequest{
		Provider: providerKey,
		FileName: "tencent-workbuddy.json",
		RawJSON:  []byte(`{"type":"tencent","kind":"workbuddy","excluded_models":["minimax-m2.7","kimi-k3"]}`),
	})
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	fmt.Printf("handled=%v metadata=%v\n", resp.Handled, resp.Auth.Metadata)
}
