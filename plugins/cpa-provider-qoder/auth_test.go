package main

import (
	"testing"

	"github.com/sivan/cpa-plugins/internal/pluginkit"
)

// TestParseAuthPreservesUserManagedFields covers the visibility-save flow: the
// management center writes excluded_models into the auth file, and the host
// re-serialises whatever ParseAuth puts on AuthData.Metadata. Dropping it would
// erase the user's model visibility choices on the next re-synthesis.
func TestParseAuthPreservesUserManagedFields(t *testing.T) {
	g := newGateway()
	resp, errParse := g.ParseAuth(t.Context(), pluginkit.AuthParseRequest{
		Provider: providerKey,
		FileName: providerKey + ".json",
		RawJSON: []byte(`{
			"type": "qoder",
			"pat": "pt-test",
			"region": "china",
			"excluded_models": ["deepseek-flash", "kimi-k3"]
		}`),
	})
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !resp.Handled {
		t.Fatal("the plugin must claim its own auth file")
	}
	stored := resp.Auth.Metadata
	excluded, okExcluded := stored["excluded_models"].([]any)
	if !okExcluded || len(excluded) != 2 {
		t.Fatalf("excluded_models did not round trip: %v", stored["excluded_models"])
	}
	if _, hasPAT := stored["pat"]; hasPAT {
		t.Fatal("the PAT must not be duplicated into metadata")
	}
}
