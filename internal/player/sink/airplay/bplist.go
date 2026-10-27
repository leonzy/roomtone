package airplay

import (
	"fmt"

	"howett.net/plist"
)

// EncodeBplist 编码 binary plist（map[string]any ↔ bplist bytes）。
//
// AirPlay 2 的 pair-setup-pin 报文与元数据推送（application/x-apple-
// binary-plist）使用。[]byte 映射为 plist 的 data 类型。
func EncodeBplist(v any) ([]byte, error) {
	return plist.Marshal(v, plist.BinaryFormat)
}

// DecodeBplist 解码 binary plist 到 map[string][]byte（data 值形态）。
//
// 值不是 data（协议形态不符）时报错，便于在配对路径上尽早发现分歧。
func DecodeBplist(data []byte) (map[string][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("airplay/bplist: 空输入")
	}
	var raw map[string]any
	if _, err := plist.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("airplay/bplist: 解码失败: %w", err)
	}
	out := make(map[string][]byte, len(raw))
	for k, v := range raw {
		b, ok := v.([]byte)
		if !ok {
			return nil, fmt.Errorf("airplay/bplist: 键 %q 的值不是 data（%T）", k, v)
		}
		out[k] = b
	}
	return out, nil
}
