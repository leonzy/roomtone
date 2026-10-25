package app

import "encoding/json"

// jsonUnmarshal 小包装，便于在 app 包内多处复用并统一错误信息。
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
