package app

import (
	"context"

	"songloft/internal/handlers"
	"songloft/internal/mcpserver"
	"songloft/internal/models"
	"songloft/internal/player"
)

type mcpSpeakerAdmin struct {
	handler *handlers.PlaybackHandler
}

func (a mcpSpeakerAdmin) AddSpeaker(ctx context.Context, kind, name, address, deviceID, publicKey string) (player.Player, error) {
	return a.handler.AddSpeaker(ctx, kind, name, address, deviceID, publicKey)
}

func (a mcpSpeakerAdmin) ReconnectSpeaker(ctx context.Context, id string) (player.Player, error) {
	return a.handler.ReconnectSpeaker(ctx, id)
}

func (a *App) initMCP() error {
	settings := models.MCPSettings{Scopes: []string{mcpserver.ScopeRead}}
	_ = a.configService.GetJSON("mcp", &settings)
	if a.config.MCPEnabled {
		settings.Enabled = true
		settings.Scopes = a.config.MCPScopes
	}
	if a.config.MCPToken != "" {
		settings.Token = a.config.MCPToken
	}
	server, err := mcpserver.New(mcpserver.Config{Enabled: settings.Enabled, Token: settings.Token, Scopes: settings.Scopes}, mcpserver.Deps{
		Songs: a.songService, Playlists: a.playlistService, Players: a.playerManager,
		Discovery: a.playerDiscovery, SpeakerAdmin: mcpSpeakerAdmin{handler: a.playbackHandler},
		Alarms: a.db.AlarmRepository(), TriggerAlarm: a.alarmRunner.Trigger, Configs: a.configService,
		History: a.listeningHistory, Recommend: a.recommendation, Metadata: a.metadataUpdate,
	})
	if err != nil {
		return err
	}
	a.mcpServer = server
	return nil
}

func (a *App) currentMCPSettings() models.MCPSettings {
	cfg := a.mcpServer.CurrentConfig()
	return models.MCPSettings{Enabled: cfg.Enabled, Token: cfg.Token, Scopes: cfg.Scopes}
}

func (a *App) applyMCPSettings(settings models.MCPSettings) error {
	return a.mcpServer.UpdateConfig(mcpserver.Config{Enabled: settings.Enabled, Token: settings.Token, Scopes: settings.Scopes})
}
