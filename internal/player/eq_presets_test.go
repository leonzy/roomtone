package player

import "testing"

// 预设必须是"拿来即用"的：10 段、钳在 ±12dB、preamp 与最大正增益匹配。
func TestEQPresetsAreUsable(t *testing.T) {
	presets := EQPresets()
	if len(presets) < 8 {
		t.Fatalf("预设太少：%d", len(presets))
	}
	seen := map[string]bool{}
	for _, p := range presets {
		if p.ID == "" || p.Name == "" {
			t.Fatalf("预设缺 id/name：%+v", p)
		}
		if seen[p.ID] {
			t.Fatalf("预设 id 重复：%s", p.ID)
		}
		seen[p.ID] = true
		if len(p.Bands) != EQBandCount {
			t.Fatalf("%s 段数 %d，应为 %d", p.ID, len(p.Bands), EQBandCount)
		}
		maxGain := 0.0
		for _, g := range p.Bands {
			if g > 12 || g < -12 {
				t.Fatalf("%s 增益 %v 越界", p.ID, g)
			}
			if g > maxGain {
				maxGain = g
			}
		}
		if maxGain > 0 && p.Preamp > -maxGain+0.2 {
			t.Fatalf("%s preamp %.2f 不足以抵消最大增益 %.2f（会削波）", p.ID, p.Preamp, maxGain)
		}
	}
	if !seen["flat"] {
		t.Fatal("必须有 flat 预设")
	}
	flat, _ := EQPresetByID("flat")
	for _, g := range flat.Bands {
		if g != 0 {
			t.Fatalf("flat 必须全 0：%v", flat.Bands)
		}
	}
	if _, ok := EQPresetByID("no-such-preset"); ok {
		t.Fatal("未知预设不能返回 true")
	}
}

// 每次返回新切片：调用方改预设不能污染全局表。
func TestEQPresetsAreCopies(t *testing.T) {
	a := EQPresets()
	a[1].Bands[0] = 99
	b := EQPresets()
	if b[1].Bands[0] == 99 {
		t.Fatal("预设表被调用方改坏了")
	}
}
