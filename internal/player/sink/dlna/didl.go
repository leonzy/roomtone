package dlna

import (
	"fmt"
	"strings"
)

// Item 一条要推给设备的媒体。
type Item struct {
	Title        string
	Artist       string
	Album        string
	URL          string
	MimeType     string
	Duration     string // H:MM:SS，可空
	ID           string
	ParentID     string
	ProtocolInfo string // 留空按 MimeType 生成
}

// BuildMetadata 生成 DIDL-Lite。
//
// 设备只把这段 XML 当"显示信息 + 协议声明"，真正的流地址是
// CurrentURI 参数；两者不一致时以 CurrentURI 为准（很多设备忽略元数据）。
func BuildMetadata(it Item) string {
	id := it.ID
	if id == "" {
		id = "1"
	}
	parent := it.ParentID
	if parent == "" {
		parent = "0"
	}
	proto := it.ProtocolInfo
	if proto == "" {
		mime := it.MimeType
		if mime == "" {
			mime = "audio/wav"
		}
		proto = fmt.Sprintf("http-get:*:%s:*", mime)
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" `)
	b.WriteString(`xmlns:dc="http://purl.org/dc/elements/1.1/" `)
	b.WriteString(`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	fmt.Fprintf(&b, `<item id="%s" parentID="%s" restricted="1">`, xmlEscape(id), xmlEscape(parent))
	fmt.Fprintf(&b, "<dc:title>%s</dc:title>", xmlEscape(it.Title))
	if it.Artist != "" {
		fmt.Fprintf(&b, "<dc:creator>%s</dc:creator>", xmlEscape(it.Artist))
	}
	if it.Album != "" {
		fmt.Fprintf(&b, "<upnp:album>%s</upnp:album>", xmlEscape(it.Album))
	}
	b.WriteString("<upnp:class>object.item.audioItem.musicTrack</upnp:class>")
	b.WriteString(`<res protocolInfo="` + xmlEscape(proto) + `"`)
	if it.Duration != "" {
		fmt.Fprintf(&b, ` duration="%s"`, xmlEscape(it.Duration))
	}
	fmt.Fprintf(&b, ">%s</res>", xmlEscape(it.URL))
	b.WriteString("</item></DIDL-Lite>")
	return b.String()
}
