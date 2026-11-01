package dlna

import (
	"context"
	"encoding/xml"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// VolumeRange is the native RenderingControl range, not an acoustic dB curve.
type VolumeRange struct{ Min, Max, Step int }

func (v VolumeRange) native(percent int) int {
	if v.Max <= v.Min {
		v = VolumeRange{0, 100, 1}
	}
	if v.Step < 1 {
		v.Step = 1
	}
	n := v.Min + int(math.Round(float64(percent)*float64(v.Max-v.Min)/100/float64(v.Step)))*v.Step
	if n > v.Max {
		n = v.Max
	}
	return n
}

// LoadVolumeRange is best effort: absent/broken SCPD retains standard 0..100.
func LoadVolumeRange(ctx context.Context, cl *Client, dev *Device) {
	svc := dev.Services[SvcRenderingControl]
	if svc.SCPDURL == nil {
		return
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.SCPDURL.String(), nil)
	if err != nil {
		return
	}
	hc := cl.HC
	if hc == nil {
		hc = NewClient().HC
	}
	resp, err := hc.Do(r)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var scpd struct {
		Variables []struct {
			Name  string `xml:"name"`
			Range struct {
				Min  string `xml:"minimum"`
				Max  string `xml:"maximum"`
				Step string `xml:"step"`
			} `xml:"allowedValueRange"`
		} `xml:"serviceStateTable>stateVariable"`
	}
	if xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&scpd) != nil {
		return
	}
	for _, variable := range scpd.Variables {
		if variable.Name != "Volume" {
			continue
		}
		min, e1 := strconv.Atoi(strings.TrimSpace(variable.Range.Min))
		max, e2 := strconv.Atoi(strings.TrimSpace(variable.Range.Max))
		step, _ := strconv.Atoi(strings.TrimSpace(variable.Range.Step))
		if step < 1 {
			step = 1
		}
		if e1 == nil && e2 == nil && min >= 0 && max > min && max <= 65535 && step <= max-min {
			dev.VolumeRange = VolumeRange{min, max, step}
		}
	}
}
