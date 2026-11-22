package services

import "fmt"

func channelRoute(modes []string) string {
	if len(modes) == 0 || modes[0] == "stereo" {
		return ""
	}
	return modes[0]
}

// ChannelRouteFilter duplicates the selected channel into both speaker drivers.
// Named source channels absent in a recording remain silent (FFmpeg pan semantics).
func ChannelRouteFilter(mode string) (string, error) {
	if mode == "" || mode == "stereo" {
		return "", nil
	}
	if mode == "mono" {
		return "aformat=channel_layouts=stereo,pan=stereo|c0=0.5*c0+0.5*c1|c1=0.5*c0+0.5*c1", nil
	}
	if mode == "left" || mode == "right" {
		ch := "c0"
		if mode == "right" {
			ch = "c1"
		}
		return "aformat=channel_layouts=stereo,pan=stereo|c0=" + ch + "|c1=" + ch, nil
	}
	channels := map[string]string{"front_left": "FL", "front_right": "FR", "center": "FC", "lfe": "LFE", "back_left": "BL", "back_right": "BR", "back_center": "BC", "side_left": "SL", "side_right": "SR"}
	if ch, ok := channels[mode]; ok {
		return "pan=stereo|c0=" + ch + "|c1=" + ch, nil
	}
	return "", fmt.Errorf("invalid channel route %q", mode)
}
