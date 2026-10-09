package ui

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// point 是点表中的一个点。Area 是 0x、1x、3x 或 4x 数据区，Offset 是协议地址。
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

// cover 返回占用 off 的多寄存器点（32 位或字符串）：off 是它的第 2 个及以后的寄存器。
// 点之间不重叠，往前找到的第一个点就是唯一可能覆盖 off 的点。
func (t pointTable) cover(area modbus.Area, off uint16) (point, bool) {
	for d := 1; d <= (maxStringLen+1)/2 && d <= int(off); d++ {
		if p, ok := t.get(area, off-uint16(d)); ok {
			return p, d < p.regs()
		}
	}
	return point{}, false
}

func (t pointTable) occupied(area modbus.Area, off uint16) bool {
	_, ok := t.cover(area, off)
	return ok
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
	"BOOL": modbus.TypeBool, "BOOLEAN": modbus.TypeBool,
	"INT16": modbus.TypeInt16, "INT": modbus.TypeInt16, "SHORT": modbus.TypeInt16,
	"UINT16": modbus.TypeUint16, "UINT": modbus.TypeUint16, "WORD": modbus.TypeUint16,
	"INT32": modbus.TypeInt32, "DINT": modbus.TypeInt32, "LONG": modbus.TypeInt32,
	"UINT32": modbus.TypeUint32, "UDINT": modbus.TypeUint32, "DWORD": modbus.TypeUint32,
	"FLOAT32": modbus.TypeFloat32, "FLOAT": modbus.TypeFloat32, "REAL": modbus.TypeFloat32,
	"INT64": modbus.TypeInt64, "LINT": modbus.TypeInt64,
	"UINT64": modbus.TypeUint64, "ULINT": modbus.TypeUint64, "LWORD": modbus.TypeUint64, "QWORD": modbus.TypeUint64,
	"FLOAT64": modbus.TypeFloat64, "DOUBLE": modbus.TypeFloat64, "LREAL": modbus.TypeFloat64,
	"STRING": typeString, "STR": typeString, "CHAR": typeString,
	"FLOAT32-IEEE": modbus.TypeFloat32, "FLOAT64-IEEE": modbus.TypeFloat64, // Telegraf 的写法
}

// pointImport 是一次导入的结果。
type pointImport struct {
	format   string // 文件格式，显示给用户
	points   []point
	skipped  []string // 跳过的行和原因：平台导出的属性表里可能有布尔、日期、虚拟点位
	overlaps int      // 其中与前面的点地址重叠的行数
	notes    []string // 其他提示，例如计算公式无法换算
	noOrder  bool     // 文件里没有字节序，32 / 64 位点按标准大端（ABCD，也是 Telegraf 的默认值）
}

// parsePointsFile 解析点表文件。.xlsx 读第一个能识别的工作表，其他按 CSV。两种文件都可以是
// 本程序的点表格式（表头见 parsePointRows），也可以是物联网平台导出的设备属性表（见 parseAttrRows）。
func parsePointsFile(name string, data []byte) (pointImport, error) {
	if strings.EqualFold(filepath.Ext(name), ".xlsx") {
		sheets, err := readXLSX(data)
		if err != nil {
			return pointImport{}, err
		}
		for _, sh := range sheets {
			if len(sh.rows) > 0 && (isAttrTable(sh.rows[0]) || hasColumns(sh.rows[0], "地址", "名称", "类型")) {
				return parsePointRowsAny(sh.rows)
			}
		}
		return pointImport{}, errors.New("没有找到点表：表头应有“地址、名称、类型”（本程序的点表），或“属性标识、属性名称”（平台导出的设备属性表）")
	}
	rows, err := readCSV(data)
	if err != nil {
		return pointImport{}, err
	}
	return parsePointRowsAny(rows)
}

func parsePointRowsAny(rows [][]string) (pointImport, error) {
	if len(rows) < 2 {
		return pointImport{}, errors.New("点表至少要有表头和一行数据")
	}
	if isAttrTable(rows[0]) {
		return parseAttrRows(rows)
	}
	ps, err := parsePointRows(rows)
	return pointImport{format: "点表", points: ps}, err
}

// readCSV 读出 CSV 的全部行，兼容 Excel 另存的 UTF-8（带 BOM）和 GBK 编码。
func readCSV(data []byte) ([][]string, error) {
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
	return r.ReadAll()
}

func columns(header []string) map[string]int {
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	return col
}

func hasColumns(header []string, names ...string) bool {
	col := columns(header)
	for _, n := range names {
		if _, ok := col[n]; !ok {
			return false
		}
	}
	return true
}

func cellGetter(col map[string]int, row []string) func(string) string {
	return func(name string) string {
		if i, ok := col[name]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
}

// overlapError 是新点与已有的点地址重复或重叠，导入设备属性表时单独统计。
type overlapError string

func (e overlapError) Error() string { return string(e) }

// add 加入一个点，与已有的点重复或重叠时报错，说明是和哪个点冲突。
func (t pointTable) add(p point) error {
	ref := modbus.Reference(p.Area, p.Offset)
	if int(p.Offset)+p.regs() > 0x10000 {
		return fmt.Errorf("%s 占用 %d 个地址，超出协议地址范围", ref, p.regs())
	}
	if prev, dup := t.get(p.Area, p.Offset); dup {
		return overlapError(fmt.Sprintf("地址 %s 与“%s”重复", ref, prev.Name))
	}
	if prev, ok := t.cover(p.Area, p.Offset); ok {
		return overlapError(fmt.Sprintf("地址 %s 被“%s”占用：它是 %s，占 %s", ref, prev.Name, prev.Type, refSpan(prev.Area, prev.Offset, prev.regs())))
	}
	for k := 1; k < p.regs(); k++ {
		if next, ok := t.get(p.Area, p.Offset+uint16(k)); ok {
			return overlapError(fmt.Sprintf("%s 占用 %d 个寄存器，其中 %s 已经是“%s”", ref, p.regs(), modbus.Reference(p.Area, next.Offset), next.Name))
		}
	}
	t[ptKey{p.Area, p.Offset}] = p
	return nil
}

// parsePointRows 解析本程序格式的点表。第一行是表头，按列名取值，列的顺序随意：
//
//	地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举,长度
//	40347,温差设定,FLOAT32,CDAB,1,℃,RW,5,25,,
//	40019,补水泵状态,UINT16,,,,,,,0=停止;1=运行,
//	40901,设备型号,STRING,,,,,,,,16
//	40701,累计电能,UINT64,CDAB,0.001,kWh,R,,,
//
// 地址支持 40347、4x0347、30001、346（原始 Offset 按保持寄存器）；地址、名称、类型必填；STRING 要填长度（字符数）。
// 64 位类型的字节序可以写 ABCDEFGH 等 8 个字母，也可以按设备手册写 32 位的 ABCD / CDAB / BADC / DCBA，按同样的规则换算。
// 这是用户自己维护的文件，第一处错误就报出来让用户改对。
func parsePointRows(rows [][]string) ([]point, error) {
	col := columns(rows[0])
	for _, need := range []string{"地址", "名称", "类型"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("表头缺少“%s”列。表头应为：地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举", need)
		}
	}
	var out []point
	seen := pointTable{}
	for n, row := range rows[1:] {
		line := n + 2
		get := cellGetter(col, row)
		if get("地址") == "" && get("名称") == "" {
			continue // 空行
		}
		p, err := parsePoint(get, false)
		if err == nil {
			err = seen.add(p)
		}
		if err != nil {
			return nil, fmt.Errorf("第 %d 行：%w", line, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func isAttrTable(header []string) bool { return hasColumns(header, "属性标识", "属性名称") }

// parseAttrRows 解析物联网平台导出的设备属性表（用 Telegraf 采集 Modbus 的平台常见这种格式）：
//
//	属性标识        属性名称  读写模式  单位  计算公式  数据类型  模式
//	4x0021:REAL    二次供温  1        ℃              2        0
//
// 属性标识是“地址:类型”，也可以再带字节序（4x0021:REAL:CDAB），类型按点表的类型名和别名识别；
// 没写类型时按数据类型列：1 整数（按 INT16）、2 浮点、3 双精度、6 布尔；4 字符、5 日期暂不支持。
// 读写模式 2 表示可写；模式 1 是虚拟点位，没有 Modbus 地址；计算公式支持 x*0.1、/10 这种倍率。
// 平台导出的表里常有本程序表示不了的点，这些行跳过并说明原因，其他照常导入。
func parseAttrRows(rows [][]string) (pointImport, error) {
	col := columns(rows[0])
	imp := pointImport{format: "设备属性表", noOrder: true}
	seen := pointTable{}
	for n, row := range rows[1:] {
		get := cellGetter(col, row)
		id, name := strings.ReplaceAll(get("属性标识"), "：", ":"), get("属性名称")
		if id == "" && name == "" {
			continue
		}
		where := fmt.Sprintf("第 %d 行 %s", n+2, name)
		skip := func(why string) { imp.skipped = append(imp.skipped, where+"："+why) }
		if intText(get("模式")) == "1" {
			skip("虚拟点位，没有 Modbus 地址")
			continue
		}
		addr, rest, _ := strings.Cut(id, ":")
		typ, order, _ := strings.Cut(rest, ":")
		if typ == "" {
			typ = map[string]string{"1": "INT16", "2": "FLOAT32", "3": "FLOAT64", "6": "BOOL"}[intText(get("数据类型"))]
			if typ == "" {
				skip("没有类型，或数据类型是字符、日期，暂不支持")
				continue
			}
		}
		if order != "" {
			imp.noOrder = false
		}
		scale, ok := formulaScale(get("计算公式"))
		if !ok {
			imp.notes = append(imp.notes, fmt.Sprintf("%s：计算公式“%s”无法换算，按原始值显示", where, get("计算公式")))
		}
		rw := ""
		if intText(get("读写模式")) == "2" {
			rw = "RW"
		}
		fields := map[string]string{"地址": addr, "名称": name, "类型": typ, "字节序": order, "倍率": scale, "单位": get("单位"), "读写": rw}
		p, err := parsePoint(func(k string) string { return fields[k] }, true)
		if err == nil {
			err = seen.add(p)
		}
		if err != nil {
			if errors.As(err, new(overlapError)) {
				imp.overlaps++
			}
			skip(err.Error())
			continue
		}
		imp.points = append(imp.points, p)
	}
	if len(imp.points) == 0 {
		return imp, fmt.Errorf("设备属性表里没有可以导入的点。%s", strings.Join(imp.skipped, "；"))
	}
	return imp, nil
}

// intText 把 Excel 里的数字（常带 .0，例如 2.0）规范成整数文字。
func intText(s string) string {
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return s
}

var formulaRe = regexp.MustCompile(`^(?:[A-Za-z_$][A-Za-z0-9_${}]*)?([*/])([0-9]+(?:\.[0-9]+)?)$`)

// formulaScale 把 x*0.1、value/10、*0.01 这种计算公式换成倍率；空公式返回空（倍率 1）。
// 其他公式（带偏移、函数）ok 为 false。
func formulaScale(f string) (scale string, ok bool) {
	f = strings.NewReplacer(" ", "", "×", "*", "÷", "/").Replace(f)
	if f == "" || f == "x" || f == "X" {
		return "", true
	}
	m := formulaRe.FindStringSubmatch(f)
	if m == nil {
		return "", false
	}
	k, err := strconv.ParseFloat(m[2], 64)
	if err != nil || k == 0 {
		return "", false
	}
	if m[1] == "/" {
		k = 1 / k
	}
	return strconv.FormatFloat(k, 'g', -1, 64), true
}

func parsePoint(get func(string) string, attrTable bool) (point, error) {
	p := point{Name: get("名称"), Scale: 1, Unit: get("单位")}
	addr := strings.TrimSpace(get("地址"))
	var cands []modbus.AddressCandidate
	var err error
	// 线圈写法：设备属性表里的 0xNNNN，以及 BOOL 点的 0xNNNN、00001。其他类型的 0x 开头仍按十六进制 Offset，
	// 旧点表里的 0x0001,温度,INT16 含义不变
	isBool := typeAliases[strings.ToUpper(get("类型"))] == modbus.TypeBool
	s := strings.ToLower(addr)
	digits := func(s string) bool {
		return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
	}
	coil := ""
	switch {
	case (attrTable || isBool) && strings.HasPrefix(s, "0x") && digits(s[2:]):
		coil = s[2:]
	case isBool && (len(s) == 5 || len(s) == 6) && s[0] == '0' && digits(s):
		coil = s
	}
	if coil != "" {
		var n uint64
		n, err = strconv.ParseUint(coil, 10, 32)
		if err == nil && (n < 1 || n > 65536) {
			err = fmt.Errorf("线圈地址 %s 应为 0x0001–0x65536", addr)
		}
		if err == nil {
			cands = []modbus.AddressCandidate{{Area: modbus.AreaCoils, Offset: uint16(n - 1)}}
		}
	} else {
		cands, err = modbus.ParseAddress(addr)
	}
	if err != nil {
		return p, err
	}
	p.Area = modbus.AreaNone
	for _, c := range cands {
		if c.Area != modbus.AreaNone {
			p.Area, p.Offset = c.Area, c.Offset
			break
		}
	}
	if p.Area == modbus.AreaNone {
		p.Area, p.Offset = modbus.AreaHoldingRegisters, cands[0].Offset // 原始 Offset 按保持寄存器
	}
	if p.Name == "" {
		return p, errors.New("名称不能为空")
	}
	t, ok := typeAliases[strings.ToUpper(get("类型"))]
	if !ok {
		return p, fmt.Errorf("类型“%s”不认识，应为 BOOL、INT16、UINT16、INT32、UINT32、FLOAT32、INT64、UINT64、FLOAT64、STRING", get("类型"))
	}
	p.Type = t
	if (p.Area == modbus.AreaCoils || p.Area == modbus.AreaDiscreteInputs) && (t == typeString || t.Registers() != 1) {
		return p, fmt.Errorf("%s 位区只能使用 BOOL 或 16 位整数类型", p.Area.Prefix())
	}
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
