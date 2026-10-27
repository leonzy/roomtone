package raop2

import (
	"bytes"
	"errors"
)

// HomeKit TLV 编解码，逐字移植 Roon 的 TLV.cs。
//
// 线格式：[1B type][1B len][value]，value >255 字节按 255 分段（每段重复
// type 头）；空值编码为 [type][0]。解码时连续同 type 的段合并；非连续
// 重复的 type 后一段覆盖（对齐 C# Dictionary 索引器赋值语义）。
//
// 编码保持 Add 的插入序（对齐 C# Dictionary 的实际遍历序）——因此不用
// 裸 map（Go map 遍历序随机），而是有序 entries。
type TLV struct {
	keys []byte
	vals map[byte][]byte
}

// TLV 类型常量，逐字来自 TLVType.cs。
// 注意与常见 HAP 示例不同：State=6 而非 1、Signature=10。
const (
	TLVMethod               byte = 0
	TLVIdentifier           byte = 1
	TLVSalt                 byte = 2
	TLVPublicKey            byte = 3
	TLVProof                byte = 4
	TLVEncryptedDataWithTag byte = 5
	TLVState                byte = 6
	TLVError                byte = 7
	TLVSignature            byte = 10
	TLVFlags                byte = 19
)

// Add 追加一个值（重复 type 覆盖且保持原插入位置，对齐 C# _dict[type] = val）。
func (t *TLV) Add(typo byte, val []byte) {
	if t.vals == nil {
		t.vals = make(map[byte][]byte)
	}
	if _, exists := t.vals[typo]; !exists {
		t.keys = append(t.keys, typo)
	}
	t.vals[typo] = val
}

// Get 取值（不存在返回 nil）。
func (t TLV) Get(typo byte) []byte {
	if t.vals == nil {
		return nil
	}
	return t.vals[typo]
}

// Has 是否存在该 type。
func (t TLV) Has(typo byte) bool {
	if t.vals == nil {
		return false
	}
	_, ok := t.vals[typo]
	return ok
}

// Encode 按插入序编码；空值 → [type][0]；>255 分段。
func (t TLV) Encode() []byte {
	var buf bytes.Buffer
	for _, k := range t.keys {
		val := t.vals[k]
		if len(val) == 0 {
			buf.WriteByte(k)
			buf.WriteByte(0)
			continue
		}
		for i := 0; i < len(val); i += 255 {
			n := len(val) - i
			if n > 255 {
				n = 255
			}
			buf.WriteByte(k)
			buf.WriteByte(byte(n))
			buf.Write(val[i : i+n])
		}
	}
	return buf.Bytes()
}

// DecodeTLV 解码；截断输入报错。
func DecodeTLV(data []byte) (TLV, error) {
	var t TLV
	t.vals = make(map[byte][]byte)
	if len(data) == 0 {
		return t, nil
	}
	var cur bytes.Buffer
	var curType byte = data[0]
	flush := func(typo byte) {
		// 对齐 C#：val[tLVType] = ...（非连续重复覆盖）
		if _, exists := t.vals[typo]; !exists {
			t.keys = append(t.keys, typo)
		}
		t.vals[typo] = append([]byte(nil), cur.Bytes()...)
	}
	for i := 0; i < len(data); {
		typo := data[i]
		i++
		if i >= len(data) {
			return TLV{}, errors.New("tlv: 截断的输入（缺少长度字节）")
		}
		if typo != curType {
			flush(curType)
			curType = typo
			cur.Reset()
		}
		n := int(data[i])
		i++
		if i+n > len(data) {
			return TLV{}, errors.New("tlv: 截断的输入（值长度越界）")
		}
		cur.Write(data[i : i+n])
		i += n
	}
	flush(curType)
	return t, nil
}
