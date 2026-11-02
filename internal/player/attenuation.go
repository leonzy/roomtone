package player

import (
	"math"
	"songloft/internal/player/dsp"
)

// normalized separates the user's attenuation from the derived anti-clipping
// gain. Repeated saves and preset changes must not compound the attenuation.
func (p EQProfile) normalized() EQProfile {
	p.Bands = dsp.NormalizeBands(p.Bands)
	if math.IsNaN(p.AttenuationDB) || math.IsInf(p.AttenuationDB, 0) {
		p.AttenuationDB = 0
	}
	p.AttenuationDB = math.Max(0, math.Min(24, p.AttenuationDB))
	p.Preamp = -p.AttenuationDB
	if p.Enabled {
		p.Preamp += dsp.ComputePreamp(p.Bands)
	}
	return p
}

func (p EQProfile) effective() EQProfile {
	p = p.normalized()
	if !p.Enabled {
		p.Bands = make([]float64, EQBandCount)
	}
	return p
}
