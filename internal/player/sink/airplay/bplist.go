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

// DecodeBplistAny 解码到 map[string]any（保留原始类型：string/[]byte/...）。
// pair-setup-pin #1 的 {method:"pin", user:...} 是字符串字典。
func DecodeBplistAny(data []byte) (map[string]any, error) {
	var raw map[string]any
	if _, err := plist.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("airplay/bplist: 解码失败: %w", err)
	}
	return raw, nil
}

// DecodeBplistNumbers 解码 binary plist 到 map[string]int（数字值形态，
// 如 SETUP#1 响应的 {timingPort, eventPort}）。
func DecodeBplistNumbers(data []byte) (map[string]int, error) {
	raw, err := DecodeBplistAny(data)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(raw))
	for k, v := range raw {
		n, ok := v.(uint64)
		if !ok {
			if f, ok := v.(float64); ok {
				out[k] = int(f)
				continue
			}
			return nil, fmt.Errorf("airplay/bplist: 键 %q 的值不是数字（%T）", k, v)
		}
		out[k] = int(n)
	}
	return out, nil
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
