package raop

// airplay 共享层引用（A2P-0：alac/rtp/crypto/dmap/dacp/digest 从 raop 纯移动
// 到 airplay 包，行为零变化）。raop（V1 会话）与 raop2（V2 客户端）共用这些
// 协议件；这里用别名保持 raop 内部引用不变，取最小 diff。
//
// 注意：函数别名是 var 而非真转发，仅用于包内引用，不构成公开 API 承诺。
import "songloft/internal/player/sink/airplay"

// --- 类型（真别名，方法集与零值语义完全一致） ---

type (
	Packetizer = airplay.Packetizer
	NTPTime    = airplay.NTPTime
	DACPServer = airplay.DACPServer
	// dmapEncoder 原名保留（metadata.go 引用）。
	dmapEncoder = airplay.DMAPEncoder
)

// --- 常量 ---

const (
	PayloadType    = airplay.PayloadType
	ControlSync    = airplay.ControlSync
	ControlTiming  = airplay.ControlTiming
	NTPEpochOffset = airplay.NTPEpochOffset
	AESKeySize     = airplay.AESKeySize
)

// --- 函数 ---

var (
	EncodeALAC         = airplay.EncodeALAC
	ToNTP              = airplay.ToNTP
	BuildRTPPacket     = airplay.BuildRTPPacket
	BuildSyncPacket    = airplay.BuildSyncPacket
	BuildTimingPacket  = airplay.BuildTimingPacket
	NewDACPServer      = airplay.NewDACPServer
	NewAESKey          = airplay.NewAESKey
	EncryptAESKey      = airplay.EncryptAESKey
	ParsePublicKey     = airplay.ParsePublicKey
	EncryptFrames      = airplay.EncryptFrames
	DecryptFrames      = airplay.DecryptFrames
	LegacyPublicKeyPEM = airplay.LegacyPublicKeyPEM
	// digest 三件套原名保留（sink.go/metadata 路径引用）。
	computeDigest        = airplay.ComputeDigest
	authorizationHeader  = airplay.AuthorizationHeader
	parseWWWAuthenticate = airplay.ParseWWWAuthenticate
)
