package platform

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	ot "github.com/go-text/typesetting/font/opentype"
)

// Fyne 2.8 的字体加载器只解析单个 sfnt；把系统 TTC 中所选字体的表重排为内存中的 sfnt。
func standaloneFont(ld *ot.Loader) ([]byte, error) {
	tags := ld.Tables()
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	n := len(tags)
	if n == 0 || n > 4096 {
		return nil, fmt.Errorf("invalid font table count %d", n)
	}
	out := make([]byte, 12+n*16)
	binary.BigEndian.PutUint32(out, uint32(ld.Type))
	binary.BigEndian.PutUint16(out[4:], uint16(n))
	pow, selector := 1, 0
	for pow*2 <= n {
		pow *= 2
		selector++
	}
	binary.BigEndian.PutUint16(out[6:], uint16(pow*16))
	binary.BigEndian.PutUint16(out[8:], uint16(selector))
	binary.BigEndian.PutUint16(out[10:], uint16((n-pow)*16))
	headOffset := -1
	for i, tag := range tags {
		table, err := ld.RawTable(tag)
		if err != nil {
			return nil, err
		}
		if tag == ot.MustNewTag("head") {
			if len(table) < 12 {
				return nil, fmt.Errorf("invalid head table")
			}
			table = bytes.Clone(table)
			clear(table[8:12])
			headOffset = len(out)
		}
		record := out[12+i*16:]
		binary.BigEndian.PutUint32(record, uint32(tag))
		binary.BigEndian.PutUint32(record[4:], fontChecksum(table))
		binary.BigEndian.PutUint32(record[8:], uint32(len(out)))
		binary.BigEndian.PutUint32(record[12:], uint32(len(table)))
		out = append(out, table...)
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
	}
	if headOffset < 0 {
		return nil, fmt.Errorf("font has no head table")
	}
	binary.BigEndian.PutUint32(out[headOffset+8:], 0xB1B0AFBA-fontChecksum(out))
	return out, nil
}

func fontChecksum(data []byte) uint32 {
	var sum uint32
	for len(data) >= 4 {
		sum += binary.BigEndian.Uint32(data)
		data = data[4:]
	}
	if len(data) > 0 {
		var last [4]byte
		copy(last[:], data)
		sum += binary.BigEndian.Uint32(last[:])
	}
	return sum
}
