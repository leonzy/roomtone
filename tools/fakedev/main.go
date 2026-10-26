// Command fakedev 模拟一台 Roomtone 播放设备（local 输出端）。
//
// 用途：本机只有一块声卡时，用多个 fakedev 进程造出多个区域，
// 验证「区域拥有队列」「切区域切整个播放器」这类多设备行为。
//
// 它做三件事：
//  1. 用 admin 账号登录，换一次性 ticket，连 /api/v1/playback/events；
//  2. 发 register_player 把自己注册成 local 设备（服务端会为它建组）；
//  3. 收到 load/play/pause/stop/seek/volume 指令后按 500ms 上报状态，
//     让服务端与 UI 看到「这台设备在播什么」。
//
// 它不真的出声：没有解码、没有音频输出，只维护状态机。
//
// 用法：
//
//	go run ./tools/fakedev -id fake-study -name 书房
//	go run ./tools/fakedev -id fake-kitchen -name 厨房
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type device struct {
	mu       sync.Mutex
	songID   int64
	position int64 // 毫秒
	volume   int
	state    string // playing / paused / idle
	updated  time.Time
}

func (d *device) snapshot() (int64, int64, int, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pos := d.position
	if d.state == "playing" {
		pos += time.Since(d.updated).Milliseconds()
	}
	return d.songID, pos, d.volume, d.state
}

func (d *device) apply(cmd string, songID, positionMs int64, volume int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch cmd {
	case "load":
		d.songID = songID
		d.position = positionMs
		d.state = "paused"
	case "play":
		if d.songID != 0 {
			d.state = "playing"
		}
	case "pause":
		if d.state == "playing" {
			d.position += time.Since(d.updated).Milliseconds()
		}
		d.state = "paused"
	case "stop":
		if d.state == "playing" {
			d.position += time.Since(d.updated).Milliseconds()
		}
		d.state = "idle"
		d.position = 0
	case "seek":
		d.position = positionMs
	case "volume":
		d.volume = volume
	}
	d.updated = time.Now()
}

func main() {
	server := flag.String("server", "http://127.0.0.1:58091", "服务端地址")
	id := flag.String("id", "fake-device", "设备 ID（唯一）")
	name := flag.String("name", "模拟设备", "设备显示名")
	user := flag.String("user", "admin", "登录用户名")
	pass := flag.String("pass", "admin", "登录密码")
	flag.Parse()

	token, err := login(*server, *user, *pass)
	if err != nil {
		log.Fatalf("登录失败: %v", err)
	}
	ticket, err := issueTicket(*server, token)
	if err != nil {
		log.Fatalf("换 ticket 失败: %v", err)
	}

	wsURL := strings.Replace(*server, "http", "ws", 1) +
		"/api/v1/playback/events?ticket=" + url.QueryEscape(ticket)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("连接 WS 失败: %v", err)
	}
	defer conn.Close()

	dev := &device{volume: 50, state: "idle", updated: time.Now()}
	register := map[string]any{
		"type": "register_player",
		"id":   *id,
		"name": *name,
		"kind": "local",
		"caps": map[string]any{
			"codecs":       []string{"flac", "mp3", "aac", "wav"},
			"sample_rates": []int{44100, 48000, 96000},
			"max_channels": 2,
		},
	}
	if err := conn.WriteJSON(register); err != nil {
		log.Fatalf("注册设备失败: %v", err)
	}
	log.Printf("已注册设备 %s（%s）", *name, *id)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// 上报循环：500ms 一次，和真实客户端一致。
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			songID, pos, vol, state := dev.snapshot()
			report := map[string]any{
				"type":        "report",
				"song_id":     songID,
				"position_ms": pos,
				"volume":      vol,
				"state":       state,
			}
			if err := conn.WriteJSON(report); err != nil {
				return
			}
		}
	}()

	go func() {
		<-stop
		_ = conn.WriteJSON(map[string]any{"type": "unregister_player", "id": *id})
		_ = conn.Close()
		os.Exit(0)
	}()

	for {
		var msg struct {
			Cmd        string `json:"cmd"`
			SongID     int64  `json:"song_id"`
			PositionMs int64  `json:"position_ms"`
			Volume     int    `json:"volume"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("连接结束: %v", err)
			return
		}
		if msg.Cmd == "" {
			continue
		}
		dev.apply(msg.Cmd, msg.SongID, msg.PositionMs, msg.Volume)
		log.Printf("指令 %-6s song=%d pos=%dms vol=%d", msg.Cmd, msg.SongID, msg.PositionMs, msg.Volume)
	}
}

func login(server, user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := http.Post(server+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func issueTicket(server, token string) (string, error) {
	req, _ := http.NewRequest(http.MethodPost, server+"/api/v1/playback/events/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Ticket, nil
}
