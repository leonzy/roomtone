package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/database/testutil"
	"songloft/internal/models"
	"songloft/internal/services"
)

func TestMCPSettingsEnablePersistAndExternalGuard(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	configs := services.NewConfigService(db.ConfigRepository())
	settings := models.MCPSettings{Scopes: []string{"read"}}
	apply := func(value models.MCPSettings) error {
		settings = value
		return nil
	}
	handler := NewMCPSettingsHandler(configs, func() models.MCPSettings { return settings }, apply, false, "")
	body := []byte(`{"enabled":true,"scopes":["read","playback"]}`)
	response := httptest.NewRecorder()
	handler.UpdateMCPSettings(response, httptest.NewRequest(http.MethodPut, "/settings/mcp", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("enable failed: %d %s", response.Code, response.Body.String())
	}
	if !settings.Enabled || settings.Token == "" {
		t.Fatalf("token was not generated: %+v", settings)
	}
	var stored models.MCPSettings
	if err := configs.GetJSON("mcp", &stored); err != nil || stored.Token != settings.Token {
		t.Fatalf("settings not persisted: %+v %v", stored, err)
	}
	response = httptest.NewRecorder()
	handler.GetMCPSettings(response, httptest.NewRequest(http.MethodGet, "/settings/mcp", nil))
	var view map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["endpoint"] != "/mcp" || view["config_example"] == nil {
		t.Fatalf("missing agent guidance: %+v", view)
	}

	external := NewMCPSettingsHandler(configs, func() models.MCPSettings { return settings }, apply, true, "")
	response = httptest.NewRecorder()
	external.UpdateMCPSettings(response, httptest.NewRequest(http.MethodPut, "/settings/mcp", bytes.NewReader([]byte(`{"enabled":false}`))))
	if response.Code != http.StatusConflict {
		t.Fatalf("environment-managed setting returned %d", response.Code)
	}
}

func TestMCPSettingsValidationFailureDoesNotPersist(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	configs := services.NewConfigService(db.ConfigRepository())
	settings := models.MCPSettings{Scopes: []string{"read"}}
	handler := NewMCPSettingsHandler(configs, func() models.MCPSettings { return settings }, func(models.MCPSettings) error {
		return context.Canceled
	}, false, "")
	response := httptest.NewRecorder()
	handler.UpdateMCPSettings(response, httptest.NewRequest(http.MethodPut, "/settings/mcp", bytes.NewReader([]byte(`{"enabled":true}`))))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid update returned %d", response.Code)
	}
	if _, err := configs.GetConfig(context.Background(), "mcp"); err == nil {
		t.Fatal("failed update must not persist")
	}
}
