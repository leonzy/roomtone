// Package ws 是播放事件的 WebSocket 服务端。
//
// 仓库里原本只有 gorilla/websocket 的**客户端**用法（jsplugin 连外部插件），
// 服务端 hub 是这次新建的。两个必须点：
//   - 鉴权走一次性 ticket，不把 JWT 放 URL（会落访问日志）；
//   - 广播永不阻塞，慢客户端丢自己的副本（丢弃计数进指标）。
package ws

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"songloft/internal/player"
)

// TicketTTL ticket 有效期。
const TicketTTL = 30 * time.Second

// ErrTicketInvalid ticket 无效 / 过期 / 已用过。
var ErrTicketInvalid = errors.New("ticket invalid or expired")

type ticket struct {
	user    string
	expires time.Time
}

// TicketStore 一次性 ticket。
//
// 为什么必须一次性：ticket 在 URL 里，浏览器历史、反代日志、Referer 都可能泄露。
// 30 秒 + 用后即焚，泄露窗口缩到"拿到的那一刻恰好被重放"。
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ticket
	now     func() time.Time
}

// NewTicketStore 创建 ticket 仓库。
func NewTicketStore() *TicketStore {
	return &TicketStore{tickets: make(map[string]ticket), now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (s *TicketStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Issue 为用户签发一个 ticket。
func (s *TicketStore) Issue(user string) (string, time.Time, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 顺手清理过期项，避免长期运行的进程里 map 只增不减。
	now := s.now()
	for k, v := range s.tickets {
		if now.After(v.expires) {
			delete(s.tickets, k)
		}
	}
	exp := now.Add(TicketTTL)
	s.tickets[token] = ticket{user: user, expires: exp}
	return token, exp, nil
}

// Redeem 兑换 ticket，成功即销毁。
func (s *TicketStore) Redeem(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[token]
	if !ok {
		return "", ErrTicketInvalid
	}
	delete(s.tickets, token)
	if s.now().After(t.expires) {
		return "", ErrTicketInvalid
	}
	return t.user, nil
}

// CommandSender 客户端连接的指令下发能力（local sink 的 Controller 实现）。
type CommandSender interface {
	SendCommand(cmd interface{}) error
}

// Client 一条 WS 连接。
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	user     string
	playerID string

	send   chan []byte
	closed chan struct{}
	once   sync.Once
}

// SendCommand 把指令编码成 JSON 下发。
//
// 用带缓冲的 send 通道 + 非阻塞写：客户端卡住时丢指令而不是卡住引擎的
// 分发循环。丢掉的指令会在客户端重连后靠 /playback 全量对齐补上。
func (c *Client) SendCommand(cmd interface{}) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	return c.SendRaw(data)
}

// SendRaw 发送已编码的消息。
func (c *Client) SendRaw(data []byte) error {
	select {
	case <-c.closed:
		return errors.New("client closed")
	default:
	}
	select {
	case c.send <- data:
		return nil
	default:
		c.hub.dropped.Add(1)
		return errors.New("client send buffer full")
	}
}

// PlayerID 该连接绑定的播放设备 id（未注册时为空）。
func (c *Client) PlayerID() string {
	c.hub.mu.RLock()
	defer c.hub.mu.RUnlock()
	return c.playerID
}

// SetPlayerID 绑定设备。
func (c *Client) SetPlayerID(id string) {
	c.hub.mu.Lock()
	c.playerID = id
	c.hub.mu.Unlock()
}

// Hub WS 服务端。
type Hub struct {
	bus     *player.EventBus
	tickets *TicketStore
	logger  *slog.Logger
	onMsg   func(c *Client, payload []byte)

	mu      sync.RWMutex
	clients map[*Client]struct{}
	byID    map[string]*Client

	dropped atomicCounter

	upgrader websocket.Upgrader
}

type atomicCounter struct {
	mu sync.Mutex
	n  uint64
}

func (a *atomicCounter) Add(n uint64) {
	a.mu.Lock()
	a.n += n
	a.mu.Unlock()
}

// Value 当前计数。
func (a *atomicCounter) Value() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// NewHub 创建 hub。
func NewHub(bus *player.EventBus, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Hub{
		bus:     bus,
		tickets: NewTicketStore(),
		logger:  logger,
		clients: make(map[*Client]struct{}),
		byID:    make(map[string]*Client),
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		// 同源校验：允许同源与非浏览器客户端（无 Origin）。
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			host := r.Host
			return origin == "http://"+host || origin == "https://"+host
		},
	}
	go h.pump()
	return h
}

// Tickets 暴露 ticket 仓库（handler 签发用）。
func (h *Hub) Tickets() *TicketStore { return h.tickets }

// OnMessage 注册客户端消息回调。
func (h *Hub) OnMessage(fn func(c *Client, payload []byte)) {
	h.mu.Lock()
	h.onMsg = fn
	h.mu.Unlock()
}

// ClientCount 在线连接数（指标用）。
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Dropped 因缓冲满被丢弃的消息数。
func (h *Hub) Dropped() uint64 { return h.dropped.Value() }

// pump 订阅事件总线并广播。
func (h *Hub) pump() {
	ch, cancel := h.bus.Subscribe()
	defer cancel()
	for ev := range ch {
		data, err := ev.EventJSON()
		if err != nil {
			continue
		}
		h.mu.RLock()
		clients := make([]*Client, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.mu.RUnlock()
		for _, c := range clients {
			// 慢客户端丢弃，不阻塞其它客户端。
			if err := c.SendRaw(data); err != nil {
				h.dropped.Add(1)
			}
		}
	}
}

// ServeHTTP 升级连接。
//
// ticket 从 query 取；非法直接 401，不升级。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("ticket")
	user, err := h.tickets.Redeem(token)
	if err != nil {
		http.Error(w, "invalid ticket", http.StatusUnauthorized)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("ws upgrade failed", "err", err)
		return
	}
	c := &Client{hub: h, conn: conn, user: user, send: make(chan []byte, 64), closed: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go h.writeLoop(c)
	go h.readLoop(c)
}

// RegisterDevice 把设备 id 绑定到连接，供 local sink 下发指令。
func (h *Hub) RegisterDevice(c *Client, playerID string) {
	h.mu.Lock()
	if prev, ok := h.byID[playerID]; ok && prev != c {
		// 同一设备重连：老连接作废，避免指令发到死连接上。
		_ = prev.conn.Close()
	}
	c.playerID = playerID
	h.byID[playerID] = c
	h.mu.Unlock()
}

// Sender 取某设备的指令通道。
func (h *Hub) Sender(playerID string) (CommandSender, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.byID[playerID]
	if !ok {
		return nil, false
	}
	return c, true
}

func (h *Hub) readLoop(c *Client) {
	defer h.drop(c)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		h.mu.RLock()
		fn := h.onMsg
		h.mu.RUnlock()
		if fn != nil {
			fn(c, data)
		}
	}
}

func (h *Hub) writeLoop(c *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case data, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Hub) drop(c *Client) {
	c.once.Do(func() {
		close(c.closed)
		h.mu.Lock()
		delete(h.clients, c)
		if c.playerID != "" && h.byID[c.playerID] == c {
			delete(h.byID, c.playerID)
		}
		h.mu.Unlock()
		_ = c.conn.Close()
	})
}
