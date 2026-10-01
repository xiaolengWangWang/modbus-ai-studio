package ui

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// point 是点表中的一个点。Area 为保持寄存器（4x）或输入寄存器（3x），Offset 是协议地址。
type point struct {
	Area   modbus.Area
	Offset uint16
	Name   string
	Type   modbus.DataType
	Order  modbus.ByteOrder
	Scale  float64
	Unit   string         `json:",omitempty"`
	RW     bool           `json:",omitempty"`
	Min    *float64       `json:",omitempty"` // 可写点允许的工程值范围，Min、Max 都有才检查
	Max    *float64       `json:",omitempty"`
	Enum   map[int]string `json:",omitempty"` // 状态类点的取值含义
	Len    int            `json:",omitempty"` // STRING 的字符数（字节数）
}

// typeString 是点表里的字符串类型：每个寄存器两个字符，高字节在前（字节序 AB），BA 表示每个寄存器内字节交换。
const typeString modbus.DataType = "STRING"

// maxStringLen 是字符串点的最大字符数，一次读取最多 125 个寄存器。
const maxStringLen = 240

// regs 返回点占用的寄存器数。
func (p point) regs() int {
	if p.Type == typeString {
		return (p.Len + 1) / 2
	}
	return p.Type.Registers()
}

// decodeString 把字符串点的寄存器解成文字：遇到 0 结束，去掉末尾空格。
func decodeString(order modbus.ByteOrder, regs []uint16) string {
	b := modbus.RegistersToBytes(regs)
	if order == modbus.OrderBA {
		for i := 0; i+1 < len(b); i += 2 {
			b[i], b[i+1] = b[i+1], b[i]
		}
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return printableText(bytes.TrimRight(b, " "))
}

func (p point) limits() (lo, hi float64, ok bool) {
	if p.Min == nil || p.Max == nil {
		return 0, 0, false
	}
	return *p.Min, *p.Max, true
}

type ptKey struct {
	area modbus.Area
	off  uint16
}

// pointTable 是一个工作区的点表。每个主窗口各有一份，初始为空。
type pointTable map[ptKey]point

func newPointTable(ps []point) pointTable {
	t := pointTable{}
	for _, p := range ps {
		t[ptKey{p.Area, p.Offset}] = p
	}
	return t
}

func (t pointTable) get(area modbus.Area, off uint16) (point, bool) {
	p, ok := t[ptKey{area, off}]
	return p, ok
}

// occupied 表示 off 是某个多寄存器点（32 位或字符串）的第 2 个及以后的寄存器。
// 点之间不重叠，往前找到的第一个点就是唯一可能覆盖 off 的点。
func (t pointTable) occupied(area modbus.Area, off uint16) bool {
	for d := 1; d <= (maxStringLen+1)/2 && d <= int(off); d++ {
		if p, ok := t.get(area, off-uint16(d)); ok {
			return d < p.regs()
		}
	}
	return false
}

// list 按数据区、地址排序返回全部点，用于保存工作区。
func (t pointTable) list() []point {
	out := make([]point, 0, len(t))
	for _, p := range t {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		return out[i].Offset < out[j].Offset
	})
	return out
}

// demoPoints 是换热站示例点表，与内置模拟器、设计文档 13.5 一致。
func demoPoints() pointTable {
	var ps []point
	for _, s := range simulator.HeatStationPoints {
		p := point{Area: modbus.AreaHoldingRegisters, Offset: s.Offset, Name: s.Name, Type: s.Type, Order: s.Order,
			Scale: s.Scale, Unit: s.Unit, RW: s.RW, Enum: simulator.HeatStationEnums[s.Offset]}
		if lim, ok := simulator.HeatStationLimits[s.Offset]; ok {
			p.Min, p.Max = &lim[0], &lim[1]
		}
		ps = append(ps, p)
	}
	return newPointTable(ps)
}

var typeAliases = map[string]modbus.DataType{
	"INT16": modbus.TypeInt16, "INT": modbus.TypeInt16, "SHORT": modbus.TypeInt16,
	"UINT16": modbus.TypeUint16, "UINT": modbus.TypeUint16, "WORD": modbus.TypeUint16,
	"INT32": modbus.TypeInt32, "DINT": modbus.TypeInt32, "LONG": modbus.TypeInt32,
	"UINT32": modbus.TypeUint32, "UDINT": modbus.TypeUint32, "DWORD": modbus.TypeUint32,
	"FLOAT32": modbus.TypeFloat32, "FLOAT": modbus.TypeFloat32, "REAL": modbus.TypeFloat32,
	"INT64": modbus.TypeInt64, "LINT": modbus.TypeInt64,
	"UINT64": modbus.TypeUint64, "ULINT": modbus.TypeUint64, "LWORD": modbus.TypeUint64, "QWORD": modbus.TypeUint64,
	"FLOAT64": modbus.TypeFloat64, "DOUBLE": modbus.TypeFloat64, "LREAL": modbus.TypeFloat64,
	"STRING": typeString, "STR": typeString, "CHAR": typeString,
}

// parsePointsCSV 解析 CSV 点表。第一行是表头，按列名取值，列的顺序随意：
//
//	地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举,长度
//	40347,温差设定,FLOAT32,CDAB,1,℃,RW,5,25,,
//	40019,补水泵状态,UINT16,,,,,,,0=停止;1=运行,
//	40901,设备型号,STRING,,,,,,,,16
//	40701,累计电能,UINT64,CDAB,0.001,kWh,R,,,
//
// 地址支持 40347、4x0347、30001、346（原始 Offset 按保持寄存器）；地址、名称、类型必填；STRING 要填长度（字符数）。
// 64 位类型的字节序可以写 ABCDEFGH 等 8 个字母，也可以按设备手册写 32 位的 ABCD / CDAB / BADC / DCBA，按同样的规则换算。
// 兼容 Excel 另存的 UTF-8（带 BOM）和 GBK 编码。
func parsePointsCSV(data []byte) ([]point, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if !utf8.Valid(data) {
		d, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err != nil {
			return nil, errors.New("文件既不是 UTF-8 也不是 GBK 编码")
		}
		data = d
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, errors.New("点表至少要有表头和一行数据")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"地址", "名称", "类型"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("表头缺少“%s”列。表头应为：地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举", need)
		}
	}
	var out []point
	seen := pointTable{}
	for n, row := range rows[1:] {
		line := n + 2
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
			return ""
		}
		if get("地址") == "" && get("名称") == "" {
			continue // 空行
		}
		p, err := parsePoint(get)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行：%w", line, err)
		}
		ref := modbus.Reference(p.Area, p.Offset)
		if _, dup := seen.get(p.Area, p.Offset); dup || seen.occupied(p.Area, p.Offset) {
			return nil, fmt.Errorf("第 %d 行：地址 %s 与前面的点重复，或被前面的多寄存器点（32 / 64 位、字符串）占用", line, ref)
		}
		for k := 1; k < p.regs(); k++ {
			if next, ok := seen.get(p.Area, p.Offset+uint16(k)); ok {
				return nil, fmt.Errorf("第 %d 行：%s 占用 %d 个寄存器，其中 %s 已经是“%s”", line, ref, p.regs(), modbus.Reference(p.Area, next.Offset), next.Name)
			}
		}
		seen[ptKey{p.Area, p.Offset}] = p
		out = append(out, p)
	}
	return out, nil
}

func parsePoint(get func(string) string) (point, error) {
	p := point{Name: get("名称"), Scale: 1, Unit: get("单位")}
	cands, err := modbus.ParseAddress(get("地址"))
	if err != nil {
		return p, err
	}
	p.Area = modbus.AreaNone
	for _, c := range cands {
		if c.Area == modbus.AreaHoldingRegisters || c.Area == modbus.AreaInputRegisters {
			p.Area, p.Offset = c.Area, c.Offset
			break
		}
	}
	if p.Area == modbus.AreaNone {
		if cands[0].Area != modbus.AreaNone {
			return p, fmt.Errorf("地址 %s 是 %s 区，点表只支持保持寄存器（4x）和输入寄存器（3x）", get("地址"), cands[0].Area.Prefix())
		}
		p.Area, p.Offset = modbus.AreaHoldingRegisters, cands[0].Offset // 原始 Offset 按保持寄存器
	}
	if p.Name == "" {
		return p, errors.New("名称不能为空")
	}
	t, ok := typeAliases[strings.ToUpper(get("类型"))]
	if !ok {
		return p, fmt.Errorf("类型“%s”不认识，应为 INT16、UINT16、INT32、UINT32、FLOAT32、INT64、UINT64、FLOAT64、STRING", get("类型"))
	}
	p.Type = t
	p.Order = modbus.ByteOrder(strings.ToUpper(get("字节序")))
	switch {
	case p.Order == "":
		p.Order = modbus.OrderAB.For(t)
	case t.Registers() == 4:
		p.Order = p.Order.For(t)
	}
	if t == typeString {
		p.Len, err = strconv.Atoi(get("长度"))
		if err != nil || p.Len < 1 || p.Len > maxStringLen {
			return p, fmt.Errorf("STRING 要在“长度”列填字符数（1–%d）", maxStringLen)
		}
		if p.Order != modbus.OrderAB && p.Order != modbus.OrderBA {
			return p, fmt.Errorf("STRING 的字节序只能是 AB（高字节在前）或 BA（字节交换）")
		}
	} else if _, err := modbus.EncodeRaw(t, p.Order, 0); err != nil {
		return p, fmt.Errorf("%s 不能用字节序 %s", t, p.Order)
	}
	if s := get("倍率"); s != "" {
		if p.Scale, err = strconv.ParseFloat(s, 64); err != nil || p.Scale == 0 {
			return p, fmt.Errorf("倍率“%s”应为非零数字", s)
		}
	}
	rw := strings.ToUpper(get("读写"))
	p.RW = strings.Contains(rw, "W") || strings.Contains(rw, "写")
	for _, f := range []struct {
		name string
		dst  **float64
	}{{"最小", &p.Min}, {"最大", &p.Max}} {
		if s := get(f.name); s != "" {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return p, fmt.Errorf("%s“%s”不是数字", f.name, s)
			}
			*f.dst = &v
		}
	}
	if lo, hi, ok := p.limits(); ok && lo > hi {
		return p, fmt.Errorf("最小 %v 大于最大 %v", lo, hi)
	}
	if s := strings.NewReplacer("；", ";", "：", "=", ":", "=", "＝", "=").Replace(get("枚举")); s != "" {
		p.Enum = map[int]string{}
		for _, kv := range strings.Split(s, ";") {
			if kv = strings.TrimSpace(kv); kv == "" {
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.Atoi(strings.TrimSpace(k))
			if !ok || err != nil {
				return p, fmt.Errorf("枚举“%s”应写成 0=停止;1=运行", kv)
			}
			p.Enum[n] = strings.TrimSpace(v)
		}
	}
	return p, nil
}
