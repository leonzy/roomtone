package raop2

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDiscoverFiltersServicesAndUsesTXTIdentity(t *testing.T) {
	entries := []Entry{
		{Name: `Aimee\231\154\132iPadpro._companion-link._tcp.local.`, Host: "ipad.local.", Port: 63464, TXT: []string{"rpBA=00:11:22:33:44:55"}},
		{Name: `Alan\226\128\153s\ Ipad\ 4._airplay._tcp.local.`, Host: "ipad.local.", Port: 7000, TXT: []string{"deviceid=00:11:22:33:44:55", "features=0x200"}},
		{Name: "NotAudio._airplay._tcp.local.", Host: "other.local.", Port: 7000, TXT: []string{"deviceid=00:11:22:33:44:66", "features=0x1"}},
	}
	devices, err := Discover(context.Background(), func(context.Context, string, time.Duration) ([]Entry, error) { return entries, nil }, time.Second)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices=%+v error=%v", devices, err)
	}
	if devices[0].Name != "Alan’s Ipad 4" || devices[0].DeviceID != "001122334455" {
		t.Fatalf("bad identity/display: %+v", devices[0])
	}
}

func TestDiscoverUsesResolvedAddressAndDecodesUTF8Name(t *testing.T) {
	devices, err := Discover(context.Background(), func(context.Context, string, time.Duration) ([]Entry, error) {
		return []Entry{{Name: `AB12CD345678@\229\174\162\229\142\133._airplay._tcp.local.`, Host: "speaker.local.", Addr: net.ParseIP("192.168.1.24"), Port: 7000, TXT: []string{"features=0x200"}}}, nil
	}, time.Second)
	if err != nil || len(devices) != 1 || devices[0].Name != "客厅" || devices[0].Address != "192.168.1.24:7000" {
		t.Fatalf("bad display/address: %+v %v", devices, err)
	}
}

func TestDiscoverRejectsNamesWithoutValidDeviceIdentity(t *testing.T) {
	devices, err := Discover(context.Background(), func(context.Context, string, time.Duration) ([]Entry, error) {
		return []Entry{{Name: "iPad._airplay._tcp.local.", Host: "ipad.local.", Port: 7000, TXT: []string{"features=0x200"}}}, nil
	}, time.Second)
	if err != nil || len(devices) != 0 {
		t.Fatalf("invalid RTSP identity accepted: %+v %v", devices, err)
	}
}
