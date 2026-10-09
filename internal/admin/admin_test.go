package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/omniswitch-dev/omniswitch/internal/store"
)

func TestCreateListDeleteAPIKey(t *testing.T) {
	st := newAdminTestStore(t)
	handler := New(st)

	createReq := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"local","rate_limit":3,"workspace_id":"ws_1","role":"admin"}`))
	createRec := httptest.NewRecorder()
	handler.CreateAPIKey(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("CreateAPIKey status = %d, body = %s", createRec.Code, createRec.Body.String())
	}

	var created CreateKeyResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !regexp.MustCompile(`^sk-osw-[0-9a-f]{64}$`).MatchString(created.Key) || created.APIKey.KeyHash != "" {
		t.Fatalf("created response = %+v, want raw key and redacted hash", created)
	}
	hash := sha256.Sum256([]byte(created.Key))
	stored, err := st.GetAPIKeyByHash(context.Background(), hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatalf("GetAPIKeyByHash() error = %v", err)
	}
	wantPrefix := "sk-osw-" + strings.TrimPrefix(created.Key, "sk-osw-")[:6] + "…"
	if stored.KeyPrefix != wantPrefix || created.APIKey.KeyPrefix != wantPrefix {
		t.Fatalf("stored/returned display prefix = %q/%q, want %q", stored.KeyPrefix, created.APIKey.KeyPrefix, wantPrefix)
	}
	if stored.RateLimit != 3 || stored.WorkspaceID != "ws_1" || stored.Role != "admin" {
		t.Fatalf("stored key = %+v, want rate limit 3 and workspace role", stored)
	}

	listRec := httptest.NewRecorder()
	handler.ListAPIKeys(listRec, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	if listRec.Code != http.StatusOK || !bytes.Contains(listRec.Body.Bytes(), []byte(`"keys"`)) {
		t.Fatalf("ListAPIKeys status/body = %d/%s", listRec.Code, listRec.Body.String())
	}

	deleteRec := httptest.NewRecorder()
	handler.DeleteAPIKey(deleteRec, httptest.NewRequest(http.MethodDelete, "/api/keys?id="+created.APIKey.ID, nil))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteAPIKey status = %d, body = %s", deleteRec.Code, deleteRec.Body.String())
	}
	disabled, err := st.GetAPIKeyByHash(context.Background(), hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatalf("GetAPIKeyByHash(disabled) error = %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("deleted key still enabled")
	}
}

func TestCreateAPIKeyValidation(t *testing.T) {
	handler := New(newAdminTestStore(t))
	rec := httptest.NewRecorder()
	handler.CreateAPIKey(rec, httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func newAdminTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
