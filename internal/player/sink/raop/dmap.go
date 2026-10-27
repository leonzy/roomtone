package raop

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
)

// dmapEncoder DMAP 报文编码器，逐字移植 Roon 的 DMAPEncoder.cs：
//
//   - 容器：Begin 写 4B tag + 占位 4B BE 长度，End 回填实际内容长度
//     （总长 - off - 4），支持嵌套；
//   - 字符串：4B tag + 4B BE 字节长 + UTF-8 内容；
//   - 单字节：4B tag + 4B BE(1) + 1 字节值。
//
// AirPlay 元数据推送（SET_PARAMETER application/x-dmap-tagged）使用。
type dmapEncoder struct {
	buf       bytes.Buffer
	backpatch []struct {
		off int
		tag string
	}
}

func (e *dmapEncoder) Begin(tag string) error {
	if len(tag) != 4 {
		return fmt.Errorf("dmap: tag %q 不是 4 字节", tag)
	}
	e.buf.WriteString(tag)
	e.backpatch = append(e.backpatch, struct {
		off int
		tag string
	}{off: e.buf.Len(), tag: tag})
	return e.writeInt32(0)
}

func (e *dmapEncoder) WriteString(tag, value string) error {
	if len(tag) != 4 {
		return fmt.Errorf("dmap: tag %q 不是 4 字节", tag)
	}
	e.buf.WriteString(tag)
	b := []byte(value)
	if err := e.writeInt32(len(b)); err != nil {
		return err
	}
	e.buf.Write(b)
	return nil
}

func (e *dmapEncoder) WriteU8(tag string, value byte) error {
	if len(tag) != 4 {
		return fmt.Errorf("dmap: tag %q 不是 4 字节", tag)
	}
	e.buf.WriteString(tag)
	if err := e.writeInt32(1); err != nil {
		return err
	}
	return e.buf.WriteByte(value)
}

func (e *dmapEncoder) End(tag string) error {
	if len(e.backpatch) == 0 {
		return fmt.Errorf("dmap: End(%q) 没有对应的 Begin", tag)
	}
	bp := e.backpatch[len(e.backpatch)-1]
	if bp.tag != tag {
		return fmt.Errorf("dmap: End(%q) 与 Begin(%q) 不匹配", tag, bp.tag)
	}
	e.backpatch = e.backpatch[:len(e.backpatch)-1]
	length := e.buf.Len() - bp.off - 4
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(length))
	// bytes.Buffer 没有写回接口；Bytes() 返回底层切片（下次写之前稳定），
	// 直接 copy 回填长度。
	copy(e.buf.Bytes()[bp.off:bp.off+4], b[:])
	return nil
}

// Bytes 返回编码结果；存在未闭合的容器时报错（对齐 ToArray 的行为）。
func (e *dmapEncoder) Bytes() ([]byte, error) {
	if len(e.backpatch) != 0 {
		return nil, fmt.Errorf("dmap: 还有 %d 个未闭合的容器", len(e.backpatch))
	}
	return e.buf.Bytes(), nil
}

func (e *dmapEncoder) writeInt32(i int) error {
	if i < 0 || i > 0x7fffffff {
		return fmt.Errorf("dmap: 长度越界 %s", strconv.Itoa(i))
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(i))
	_, err := e.buf.Write(b[:])
	return err
}
