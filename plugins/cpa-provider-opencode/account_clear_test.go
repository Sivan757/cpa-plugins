package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivan/cpa-plugins/internal/cpakit"
)

// TestRemovedCredentialStopsBeingUsed covers retiring an account without
// restarting the kernel. Configure reuses one account object, and load and apply
// only ever add a value, so a removed credential would otherwise survive every
// reload.
func TestRemovedCredentialStopsBeingUsed(t *testing.T) {
	dir := t.TempDir()
	state := newPluginState()
	state.account = &accountAuth{}
	gateway := &gateway{impl: state}

	full := []byte("credential_dir: \"" + dir + "\"\n" +
		"session_token: \"st_first\"\n" +
		"refresh_token: \"rt_first\"\n" +
		"org_id: \"org_first\"\n")
	if errConfigure := gateway.Configure(full); errConfigure != nil {
		t.Fatalf("first Configure: %v", errConfigure)
	}
	if !state.account.configured() {
		t.Fatal("the configured credential was not adopted")
	}

	// Reconfigure with the credential removed and nothing persisted.
	minimal := []byte("credential_dir: \"" + dir + "\"\n")
	if errConfigure := gateway.Configure(minimal); errConfigure != nil {
		t.Fatalf("second Configure: %v", errConfigure)
	}
	if state.account.configured() {
		t.Fatal("a credential removed from the configuration is still in use")
	}
	if org := state.account.org(); org != "" {
		t.Fatalf("org = %q, want it cleared too", org)
	}

	// The free tier must fall back to the keyless lane alone.
	plan, errPlan := state.planFor(context.Background(), cpakit.ExecutorRequest{
		AuthProvider:   providerKey,
		AuthAttributes: map[string]string{"tier": tierFree},
		Model:          "mimo-v2.6-flash-free",
	})
	if errPlan != nil {
		t.Fatalf("planFor: %v", errPlan)
	}
	if len(plan.lanes) != 1 || !plan.lanes[0].anonymous {
		t.Fatalf("lanes = %+v, want only the keyless lane", plan.lanes)
	}
}

// TestPersistedCredentialSurvivesAReload proves the clear does not discard a
// rotated credential. The document outranks an empty configuration, so a
// renewal still survives a reload.
func TestPersistedCredentialSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	document := accountDocument{
		AccessToken:  "st_rotated",
		RefreshToken: "rt_rotated",
		OrgID:        "org_rotated",
		ExpiresAt:    time.Now().Add(24 * time.Hour).UnixMilli(),
	}
	encoded, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		t.Fatalf("encode document: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, "opencode-account.json"), encoded, 0o600); errWrite != nil {
		t.Fatalf("write document: %v", errWrite)
	}

	state := newPluginState()
	state.account = &accountAuth{}
	gateway := &gateway{impl: state}

	// An empty configuration must not win over the persisted pair.
	if errConfigure := gateway.Configure([]byte("credential_dir: \"" + dir + "\"\n")); errConfigure != nil {
		t.Fatalf("Configure: %v", errConfigure)
	}

	access, orgID, errCredentials := state.account.credentials(context.Background())
	if errCredentials != nil {
		t.Fatalf("credentials: %v", errCredentials)
	}
	if access != "st_rotated" {
		t.Fatalf("access = %q, want the persisted token", access)
	}
	if orgID != "org_rotated" {
		t.Fatalf("orgID = %q, want the persisted workspace", orgID)
	}
}
