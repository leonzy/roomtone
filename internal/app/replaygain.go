package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"songloft/internal/player"
)

// replayGainSource 把"配置 + 标签 + 缓存"合成 player.ReplayGainSource。
//
// 设计取舍：标签在**首次播放该曲**时用 ffprobe 读一次并缓存，不在扫描阶段入库。
// 好处是存量曲库升级后立刻可用、不需要整库重扫；代价是每首歌第一次播放多一次
// ffprobe（约 30~60ms），预取路径会提前把缓存预热掉，gapless 交界不受影响。
type replayGainSource struct {
	// mode 返回当前模式与是否防削波；每次调用都读配置，改了立即生效。
	mode func() (player.ReplayGainMode, bool)
	// probe 读取某首歌的标签值；ok=false 表示读不到（远程歌曲、无标签、ffprobe 缺失）。
	probe func(ctx context.Context, songID int64) (player.ReplayGain, bool)

	mu    sync.Mutex
	cache map[int64]cachedGain
}

type cachedGain struct {
	rg player.ReplayGain
	ok bool
}

// replayGainCacheLimit 缓存条数上限（每条约 40 字节，1 万条也才几百 KB）。
const replayGainCacheLimit = 10000

// LoudnessEnabled 实现 player.LoudnessSource：ReplayGain 非 off 即视为
// 组级响度归一开启。signal_path 用它把链路标成 enhanced。
func (s *replayGainSource) LoudnessEnabled() bool {
	if s.mode == nil {
		return false
	}
	mode, _ := s.mode()
	return mode != player.ReplayGainOff && mode.Valid()
}

// GainDB 实现 player.ReplayGainSource。
func (s *replayGainSource) GainDB(ctx context.Context, songID int64) (float64, bool) {
	mode, preventClipping := s.mode()
	if mode == player.ReplayGainOff || !mode.Valid() {
		return 0, false
	}
	rg, ok := s.lookup(ctx, songID)
	if !ok {
		return 0, false
	}
	db := rg.GainDB(mode, preventClipping)
	if db == 0 {
		return 0, false
	}
	return db, true
}

// lookup 命中缓存直接返回，否则读标签并写缓存。
func (s *replayGainSource) lookup(ctx context.Context, songID int64) (player.ReplayGain, bool) {
	s.mu.Lock()
	if c, ok := s.cache[songID]; ok {
		s.mu.Unlock()
		return c.rg, c.ok
	}
	s.mu.Unlock()

	if s.probe == nil {
		return player.ReplayGain{}, false
	}
	rg, ok := s.probe(ctx, songID)
	if ok && rg.Empty() {
		// 读到了文件但没有 ReplayGain 标签：同样缓存，避免每次播放都再探一次。
		ok = false
	}

	s.mu.Lock()
	if s.cache == nil || len(s.cache) >= replayGainCacheLimit {
		s.cache = make(map[int64]cachedGain, replayGainCacheLimit)
	}
	s.cache[songID] = cachedGain{rg: rg, ok: ok}
	s.mu.Unlock()
	return rg, ok
}

// replayGainProbe 用 ffprobe 读一首本地曲目的 REPLAYGAIN_* 标签。
func (a *App) replayGainProbe(ctx context.Context, songID int64) (player.ReplayGain, bool) {
	if a.songService == nil {
		return player.ReplayGain{}, false
	}
	song, err := a.songService.GetByID(ctx, songID)
	if err != nil || song == nil || song.FilePath == "" {
		return player.ReplayGain{}, false
	}
	path := a.ffprobePath()
	if path == "" {
		return player.ReplayGain{}, false
	}
	out, err := exec.CommandContext(ctx, path, "-v", "error", "-show_format", "-of", "json", song.FilePath).Output()
	if err != nil {
		return player.ReplayGain{}, false
	}
	var probe struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return player.ReplayGain{}, false
	}
	return parseReplayGainTags(probe.Format.Tags), true
}

// parseReplayGainTags 从 ffprobe 的 tags 里取 ReplayGain 四项。
//
// ffprobe 的键大小写随容器不同（FLAC/Vorbis 全大写，MP4 原样保留），
// 这里统一按大写匹配；值形如 "-7.23 dB" 或 "0.987"，单位不参与计算。
func parseReplayGainTags(tags map[string]string) player.ReplayGain {
	upper := make(map[string]string, len(tags))
	for k, v := range tags {
		upper[strings.ToUpper(strings.TrimSpace(k))] = v
	}
	return player.ReplayGain{
		TrackGain: parseGainValue(upper["REPLAYGAIN_TRACK_GAIN"]),
		TrackPeak: parseGainValue(upper["REPLAYGAIN_TRACK_PEAK"]),
		AlbumGain: parseGainValue(upper["REPLAYGAIN_ALBUM_GAIN"]),
		AlbumPeak: parseGainValue(upper["REPLAYGAIN_ALBUM_PEAK"]),
	}
}

// parseGainValue 解析 "−7.23 dB" / "0.98" 这类值，失败返回 0。
func parseGainValue(v string) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	v = strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(v), "DB"), " ")
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return f
}

// ffprobePath 每次从配置读，改了设置立即生效；缺省用 PATH 里的 ffprobe。
func (a *App) ffprobePath() string {
	var cfg struct {
		Path string `json:"path"`
	}
	if a.configService != nil {
		if err := a.configService.GetJSON("ffprobe_path", &cfg); err == nil && cfg.Path != "" {
			return cfg.Path
		}
	}
	return "ffprobe"
}
