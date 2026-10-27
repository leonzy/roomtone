package raat

import (
	"context"
	"testing"
	"time"
)

// TestSoodLiveProbe 真实 SOOD 发现探测（联调用，非 CI 断言）。
func TestSoodLiveProbe(t *testing.T) {
	devs, err := Discover(context.Background(), 3*time.Second, nil)
	t.Logf("Discover: %d devices, err=%v", len(devs), err)
	for _, d := range devs {
		t.Logf("  %s | %s | %s | %v:%d | raat=%s | keys=%v",
			d.UniqueID, d.Name, d.Model, d.IP, d.ControlPort, d.RaatVersion, d.CodeSigningKeys)
	}
}
