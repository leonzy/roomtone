package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/player"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeSinkBuilder struct{}

func (fakeSinkBuilder) Build(_ context.Context, p *player.Player) (sink.Sink, error) {
	return fake.New(p.ID), nil
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func TestMCPAuthenticationAndProtocol(t *testing.T) {
	server, err := New(Config{Token: "secret", Scopes: []string{ScopeRead}}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token returned %d", response.Code)
	}

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: "secret"}}
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: httpServer.URL, HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 35 {
		t.Fatalf("expected full control surface, got %d tools", len(tools.Tools))
	}
	presets, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "list_eq_presets", Arguments: map[string]any{}})
	if err != nil || presets.IsError {
		t.Fatalf("read tool failed: %+v, %v", presets, err)
	}
	forbidden, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "remove_speaker", Arguments: map[string]any{"speaker_id": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !forbidden.IsError {
		t.Fatal("speaker mutation must require speakers scope")
	}
}

func TestMCPRejectsCrossOrigin(t *testing.T) {
	server, err := New(Config{Token: "secret", Scopes: []string{ScopeRead}}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Origin", "https://attacker.example")
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request returned %d", response.Code)
	}
}

func TestMCPZoneAndVolumeControl(t *testing.T) {
	manager := player.NewManager(player.ManagerDeps{Builder: fakeSinkBuilder{}})
	for _, id := range []string{"living", "study"} {
		if err := manager.RegisterPlayer(context.Background(), player.Player{ID: id, Name: id, Kind: player.KindRAOP, MaxVolume: 80}); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New(Config{Token: "secret", Scopes: []string{ScopeRead, ScopeSpeakers}}, Deps{Players: manager})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: "secret"}}
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: httpServer.URL, HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	created, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "create_zone", Arguments: map[string]any{
		"name": "全屋", "member_ids": []string{"living", "study"}, "master_id": "living",
	}})
	if err != nil || created.IsError {
		t.Fatalf("create zone failed: %+v, %v", created, err)
	}
	zoneID := manager.GroupIDOf("living")
	if zoneID == "" || zoneID != manager.GroupIDOf("study") {
		t.Fatal("speakers were not grouped")
	}
	volume, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "set_zone_volume", Arguments: map[string]any{
		"zone_id": zoneID, "volume": 100,
	}})
	if err != nil || volume.IsError {
		t.Fatalf("set volume failed: %+v, %v", volume, err)
	}
	living, _ := manager.GetPlayer("living")
	if living.Volume != 80 {
		t.Fatalf("max_volume must clamp actual volume to 80, got %d", living.Volume)
	}
}
