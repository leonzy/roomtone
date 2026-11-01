package dlna

import "strings"

// Patch 厂商方言补丁。
//
// 同一个 UPnP 标准下各家行为差异集中在几处：Sonos 不接受空
// CurrentURIMetaData、部分设备需要 Play 后定位、少数设备 SUBSCRIBE
// 直接 500。命中不了就走标准路径，不做猜测。
type Patch struct {
	// TolerateEmptyMetadata true 表示元数据为空时不发 SetAVTransportURI 的第二个参数。
	TolerateEmptyMetadata bool
	// SeekUnit 覆盖 seek 单位，空则用 REL_TIME。
	SeekUnit string
	// Xiaomi S12 resets pre-play seeks when Play starts.
	SeekAfterPlay bool
	// DisableEvents true 表示跳过 SUBSCRIBE，直接轮询。
	DisableEvents bool
	// SkipSetNext 设备没有 SetNextAVTransportURI（无 gapless）。
	SkipSetNext bool
}

// DefaultPatch 标准 UPnP AV。
var DefaultPatch = Patch{SeekUnit: "REL_TIME"}

// vendors 按 modelName 小写子串匹配，顺序即优先级。
var vendors = []struct {
	match string
	patch Patch
}{
	{"s12", Patch{SeekUnit: "REL_TIME", SeekAfterPlay: true}},
	{"sonos", Patch{TolerateEmptyMetadata: true, SeekUnit: "REL_TIME", SkipSetNext: true}},
	{"kodi", Patch{SeekUnit: "REL_TIME", DisableEvents: false}},
	{"xbmc", Patch{SeekUnit: "REL_TIME", DisableEvents: false}},
	// TRACK_NR takes a track number, never a H:MM:SS timestamp.
	{"philips", Patch{SeekUnit: "REL_TIME", DisableEvents: true}},
	{"hisense", Patch{SeekUnit: "REL_TIME", DisableEvents: true}},
}

// PatchFor 按 modelName 取补丁。
func PatchFor(modelName string) Patch {
	m := strings.ToLower(strings.TrimSpace(modelName))
	for _, v := range vendors {
		if m != "" && strings.Contains(m, v.match) {
			return v.patch
		}
	}
	return DefaultPatch
}

// RegisterVendor 运行时追加/覆盖补丁（测试与灰度用）。
func RegisterVendor(match string, p Patch) {
	m := strings.ToLower(strings.TrimSpace(match))
	if m == "" {
		return
	}
	for i := range vendors {
		if vendors[i].match == m {
			vendors[i].patch = p
			return
		}
	}
	vendors = append([]struct {
		match string
		patch Patch
	}{{match: m, patch: p}}, vendors...)
}
