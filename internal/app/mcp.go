package app

import (
	"context"

	"songloft/internal/handlers"
	"songloft/internal/mcpserver"
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
	server, err := mcpserver.New(mcpserver.Config{Token: a.config.MCPToken, Scopes: a.config.MCPScopes}, mcpserver.Deps{
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
