package player

import "time"

// groupDelaysLocked aligns active members to the slowest configured DLNA
// renderer. DLNA calibration is an estimated acoustic lag, not extra delay
// applied to the DLNA itself. Solo playback never adds group compensation.
// m.mu must be held while reading group/player settings.
func (m *Manager) groupDelaysLocked(g *Group) map[string]time.Duration {
	result := make(map[string]time.Duration)
	active := make([]*Player, 0, len(g.Members))
	maxDelay, hasDLNA := 0, false
	for _, member := range g.Members {
		p := m.players[member.PlayerID]
		if p == nil || p.Standby {
			continue
		}
		active = append(active, p)
		if p.Kind == KindDLNA {
			hasDLNA = true
			maxDelay = max(maxDelay, p.DLNASyncDelayMs)
		}
	}
	if len(active) < 2 {
		return result
	}
	for _, p := range active {
		if hasDLNA {
			intrinsic := 0
			if p.Kind == KindDLNA {
				intrinsic = p.DLNASyncDelayMs
			}
			result[p.ID] = time.Duration(maxDelay-intrinsic) * time.Millisecond
		}
	}
	return result
}
