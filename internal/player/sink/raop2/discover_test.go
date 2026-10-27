package raop2

import (
	"context"
	"strings"
	"testing"
	"time"
)

// FeatureFlags 全量位表逐字来自 AirPlayFeatureFlags.cs。
func TestFeatureFlagsConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"SupportsAirPlayAudio", FeatureSupportsAirPlayAudio, 0x200},
		{"SupportsAudioRedundancy", FeatureSupportsAudioRedundancy, 0x800},
		{"SupportsFairPlayAuth", FeatureSupportsFairPlayAuth, 0x4000},
		{"SupportsArtwork", FeatureSupportsArtwork, 0x8000},
		{"SupportsProgress", FeatureSupportsProgress, 0x10000},
		{"SupportsDaapNowPlaying", FeatureSupportsDaapNowPlaying, 0x20000},
		{"AudioFormats_0", FeatureAudioFormats0, 0x40000},
		{"AudioFormats_1", FeatureAudioFormats1, 0x80000},
		{"AudioFormats_2", FeatureAudioFormats2, 0x100000},
		{"AudioFormats_3", FeatureAudioFormats3, 0x200000},
		{"SupportsRSAAuth", FeatureSupportsRSAAuth, 0x800000},
		{"SupportsMFIAuth", FeatureSupportsMFIAuth, 0x4000000},
		{"SupportsLegacyPairing", FeatureSupportsLegacyPairing, 0x8000000},
		{"HasUnifiedAdvertiserInfo", FeatureHasUnifiedAdvertiserInfo, 0x40000000},
		{"MaybeRequiresPairing", FeatureMaybeRequiresPairing, 0x80000000},
		{"IsCarPlay", FeatureIsCarPlay, 0x100000000},
		{"SupportsUnifiedMediaControl", FeatureSupportsUnifiedMediaControl, 0x4000000000},
		{"SupportsBufferedAudio", FeatureSupportsBufferedAudio, 0x10000000000},
		{"SupportsPTP", FeatureSupportsPTP, 0x20000000000},
		{"SupportsSystemPairing", FeatureSupportsSystemPairing, 0x80000000000},
		{"SupportsHomeKitPairing", FeatureSupportsHomeKitPairing, 0x400000000000},
		{"CoreUtilsPairing", FeatureCoreUtilsPairing, 0x1000000000000},
		{"SupportsBPlistNowPlaying", FeatureSupportsBPlistNowPlaying, 0x4000000000000},
		{"SupportsUnifiedPairingAndMFI", FeatureSupportsUnifiedPairingAndMFI, 0x8000000000000},
		{"SupportsSetPeersExtended", FeatureSupportsSetPeersExtended, 0x10000000000000},
		{"SupportsTransientPairing", FeatureSupportsTransientPairing, 0x1080000000000},
		{"SupportsCoreUtilsPairingAndEncryption", FeatureSupportsCoreUtilsPairingAndEncryption, 0x1484000000000},
		{"SupportsMultiRoom", FeatureSupportsMultiRoom, 0x8030000000A00},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x，期望 %#x", c.name, c.got, c.want)
		}
	}
	// StatusFlags（AirPlayStatusFlags.cs）
	if StatusPinRequired != 8 || StatusPasswordRequired != 0x80 ||
		StatusOneTimePairingRequired != 0x200 || StatusDeviceWasSetupForHKAccessControl != 0x400 ||
		StatusTightSyncIsGroupLeader != 0x2000 {
		t.Error("StatusFlags 常量与 AirPlayStatusFlags.cs 不符")
	}
}

// AC-P4①：_airplay._tcp TXT 解析——ft/features 双段 hex（低段、高段<<32）、
// sf/flags → StatusFlags、pw/pk/ch/sr/ss、tsid/psgid/igl 编组字段。
func TestParseAirPlayTXT(t *testing.T) {
	// features = 0x1,0x1 → 0x100000001（SupportsAirPlayAudio | IsCarPlay）
	// ft 是同义键（AirPlayDeviceData 两个都认）
	txt := []string{
		"deviceid=AA:BB:CC:DD:EE:FF",
		"model=AudioAccessory1,1",
		"ft=0x200,0x1", // 低段 0x200（SupportsAirPlayAudio）、高段 0x1（IsCarPlay）
		"sf=0x400",     // StatusFlags: DeviceWasSetupForHKAccessControl
		"flags=0x2000", // flags 与 sf 叠加：TightSyncIsGroupLeader
		"pw=true",
		"pk=fake-pem-key",
		"ch=2",
		"sr=48000",
		"ss=24",
		"tsid=tight-sync-uuid-1",
		"psgid=public-group-1",
		"igl=1",
	}
	dev, ok := ParseAirPlayTXT(txt)
	if !ok {
		t.Fatal("TXT 解析失败")
	}
	if dev.Features != 0x200|(0x1<<32) {
		t.Fatalf("Features = %#x，期望 %#x", dev.Features, uint64(0x200|0x1<<32))
	}
	if dev.StatusFlags != 0x400|0x2000 {
		t.Fatalf("StatusFlags = %#x，期望 %#x", dev.StatusFlags, 0x400|0x2000)
	}
	if !dev.RequiresPassword || dev.PublicKey != "fake-pem-key" {
		t.Fatalf("pw/pk 解析不符: %+v", dev)
	}
	if dev.Channels != 2 || dev.SampleRate != 48000 || dev.BitDepth != 24 {
		t.Fatalf("ch/sr/ss 解析不符: %+v", dev)
	}
	if dev.TightSyncUuid != "tight-sync-uuid-1" || dev.PublicGroupId != "public-group-1" {
		t.Fatalf("tsid/psgid 解析不符: %+v", dev)
	}
	if !dev.IsPublicGroupLeader {
		t.Fatal("igl=1 应为公共组长")
	}
	// 派生字段（AirPlayDeviceData.cs:377-394）
	if dev.GroupId() != "public-group-1" {
		t.Fatalf("GroupId = %q，期望 psgid 优先", dev.GroupId())
	}
	if !dev.IsGroupLeader() {
		t.Fatal("psgid 存在时 IsGroupLeader 应取 IsPublicGroupLeader")
	}
	if !dev.IsTSGroupLeader() {
		t.Fatal("StatusFlags 有 TightSyncIsGroupLeader 位")
	}

	// 无 psgid：GroupId 回退 tsid；组长取 TS 位
	dev2, _ := ParseAirPlayTXT([]string{"tsid=ts-only", "sf=0x2000"})
	if dev2.GroupId() != "ts-only" || !dev2.IsGroupLeader() {
		t.Fatalf("无 psgid 时应回退 tsid + TS 组长位: %+v", dev2)
	}
	// 无 psgid 且无 TS 位：不是组长
	dev3, _ := ParseAirPlayTXT([]string{"tsid=ts-only"})
	if dev3.IsGroupLeader() {
		t.Fatal("无 psgid 且无 TS 位为组长")
	}
	// features 键（与 ft 同义）
	dev4, _ := ParseAirPlayTXT([]string{"features=0x200,0x1"})
	if dev4.Features != 0x200|0x1<<32 {
		t.Fatalf("features 键解析不符: %#x", dev4.Features)
	}
}

// AC-P4②：编组——同 psgid 多设备合并为一台（组长为代表、成员数进 Model）。
func TestMergeGroups(t *testing.T) {
	devs := []Device{
		{Name: "HomePod 厨房", DeviceID: "AA:AA:AA:AA:AA:01", Model: "AudioAccessory1,1",
			PublicGroupId: "grp-1", IsPublicGroupLeader: false, Features: 0x200},
		{Name: "HomePod 客厅", DeviceID: "AA:AA:AA:AA:AA:02", Model: "AudioAccessory1,1",
			PublicGroupId: "grp-1", IsPublicGroupLeader: true, Features: 0x200},
		{Name: "独立音箱", DeviceID: "AA:AA:AA:AA:AA:03", Model: "AppleTV6,2",
			Features: 0x200},
	}
	merged := MergeGroups(devs)
	if len(merged) != 2 {
		t.Fatalf("合并后 = %d 台，期望 2（组 + 独立）", len(merged))
	}
	var grp, solo *Device
	for i := range merged {
		if merged[i].PublicGroupId == "grp-1" {
			grp = &merged[i]
		} else {
			solo = &merged[i]
		}
	}
	if grp == nil || solo == nil {
		t.Fatalf("合并结果不符: %+v", merged)
	}
	// 组长为代表
	if grp.DeviceID != "AA:AA:AA:AA:AA:02" || grp.Name != "HomePod 客厅" {
		t.Fatalf("组长应为代表: %+v", grp)
	}
	// 成员数进 Model
	if !strings.Contains(grp.Model, "(2)") {
		t.Fatalf("Model 应含成员数: %q", grp.Model)
	}
	if strings.Contains(solo.Model, "(2)") {
		t.Fatalf("独立设备 Model 不应含成员数: %q", solo.Model)
	}

	// 无组长：取首台并记日志（对齐计划：组长=IsGroupLeader，无组长取首台）
	devs2 := []Device{
		{Name: "A", DeviceID: "A1", PublicGroupId: "g", Features: 0x200},
		{Name: "B", DeviceID: "B2", PublicGroupId: "g", Features: 0x200},
	}
	merged2 := MergeGroups(devs2)
	if len(merged2) != 1 || merged2[0].DeviceID != "A1" {
		t.Fatalf("无组长时应取首台: %+v", merged2)
	}
}

// AC-P4③：Discover——两轮 _airplay._tcp 查询 + 注入假 Query + 编组合并。
func TestDiscoverAirPlay(t *testing.T) {
	var queries []string
	query := func(ctx context.Context, service string, timeout time.Duration) ([]Entry, error) {
		queries = append(queries, service)
		switch service {
		case "_airplay._tcp":
			return []Entry{
				{Name: "HomePod 客厅._airplay._tcp.local", Host: "192.0.2.10", Port: 7000,
					TXT: []string{"ft=0x200,0x1", "psgid=g1", "igl=1", "model=AudioAccessory1,1"}},
				{Name: "HomePod 厨房._airplay._tcp.local", Host: "192.0.2.11", Port: 7000,
					TXT: []string{"ft=0x200,0x1", "psgid=g1", "model=AudioAccessory1,1"}},
			}, nil
		}
		return nil, nil
	}
	devs, err := Discover(context.Background(), query, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 || queries[0] != "_airplay._tcp" {
		t.Fatalf("应查询 _airplay._tcp: %v", queries)
	}
	// 两台同 psgid → 合并为 1（组长为代表）
	if len(devs) != 1 {
		t.Fatalf("合并后 = %d 台，期望 1", len(devs))
	}
	if devs[0].Name != "HomePod 客厅" || devs[0].PublicGroupId != "g1" {
		t.Fatalf("组长为代表: %+v", devs[0])
	}
	if devs[0].Features != 0x200|0x1<<32 {
		t.Fatalf("Features = %#x", devs[0].Features)
	}
}
