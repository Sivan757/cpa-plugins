package pluginkit

import (
	"context"
	"encoding/json"
	"fmt"
)

// HostAuthFileEntry mirrors pluginapi.HostAuthFileEntry, the shape host.auth.list
// returns for every credential the host knows.
type HostAuthFileEntry struct {
	ID        string `json:"id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status,omitempty"`
}

// HostAuthSaveRequest mirrors pluginapi.HostAuthSaveRequest. Name must end with
// .json; JSON is the credential document to persist verbatim.
type HostAuthSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

type hostAuthListResponse struct {
	Files []HostAuthFileEntry `json:"files"`
}

// AuthList returns every auth record the host currently knows, without
// modifying them. Callers use it to make their own records idempotently: an
// existing record is never overwritten, so host-managed metadata survives.
func AuthList(ctx context.Context) ([]HostAuthFileEntry, error) {
	var resp hostAuthListResponse
	if errCall := callHost(MethodHostAuthList, map[string]any{}, &resp); errCall != nil {
		return nil, fmt.Errorf("host auth list: %w", errCall)
	}
	return resp.Files, nil
}

// AuthSave persists one plugin-owned auth record through the host. The host
// watches its auth directory, so the record becomes routable without a restart.
func AuthSave(ctx context.Context, req HostAuthSaveRequest) error {
	if req.Name == "" {
		return fmt.Errorf("host auth save: name is required")
	}
	if len(req.JSON) == 0 {
		return fmt.Errorf("host auth save: credential payload is required")
	}
	if errCall := callHost(MethodHostAuthSave, req, nil); errCall != nil {
		return fmt.Errorf("host auth save %s: %w", req.Name, errCall)
	}
	return nil
}
