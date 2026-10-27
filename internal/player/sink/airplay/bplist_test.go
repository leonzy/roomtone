package airplay

import (
	"bytes"
	"testing"

	"howett.net/plist"
)

// EncodeBplist/DecodeBplist 封装 howett.net/plist（原 github.com/dhowett/plist，
// 上游迁移了 module path，同一代码库）：AirPlay 2 的 SETUP 响应与
// pair-setup-pin 报文都是 binary plist。
func TestEncodeBplistRoundTrip(t *testing.T) {
	pk := []byte{0x01, 0x02, 0x03, 0x04}
	proof := bytes.Repeat([]byte{0xaa}, 32)
	enc, err := EncodeBplist(map[string]any{"pk": pk, "proof": proof})
	if err != nil {
		t.Fatalf("EncodeBplist: %v", err)
	}
	// 输出是 binary plist（magic 头 "bplist00"）。
	if !bytes.HasPrefix(enc, []byte("bplist00")) {
		t.Fatalf("输出不是 binary plist: %x", enc[:16])
	}
	// 用库直接解回，[]byte 语义不变。
	var back map[string]any
	if _, err := plist.Unmarshal(enc, &back); err != nil {
		t.Fatalf("plist.Unmarshal: %v", err)
	}
	if !bytes.Equal(back["pk"].([]byte), pk) || !bytes.Equal(back["proof"].([]byte), proof) {
		t.Fatalf("往返不一致: %+v", back)
	}

	// DecodeBplist：{"pk":<data>,"epk":<data>} 形态。
	dec, err := DecodeBplist(enc)
	if err != nil {
		t.Fatalf("DecodeBplist: %v", err)
	}
	if !bytes.Equal(dec["pk"], pk) || !bytes.Equal(dec["proof"], proof) || len(dec) != 2 {
		t.Fatalf("DecodeBplist 不符: %+v", dec)
	}
}

func TestDecodeBplistErrors(t *testing.T) {
	// 非法输入报错不 panic
	if _, err := DecodeBplist([]byte("not a plist")); err == nil {
		t.Fatal("非法输入应报错")
	}
	if _, err := DecodeBplist(nil); err == nil {
		t.Fatal("空输入应报错")
	}
	// 值不是 data（如 string）→ 明确报错（协议形态不符的早期发现）
	strEnc, err := EncodeBplist(map[string]any{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBplist(strEnc); err == nil {
		t.Fatal("非 data 值应报错")
	}
}
