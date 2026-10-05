package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
)

// 示例 CSV 既是给用户的模板，也必须与内置换热站点表完全一致。
func TestPointsCSV(t *testing.T) {
	data, err := os.ReadFile("../../assets/examples/heat-station-points.csv")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := parsePointsCSV(data)
	if err != nil {
		t.Fatal(err)
	}
	got, want := newPointTable(ps).list(), demoPoints().list()
	if len(got) != len(want) {
		t.Fatalf("示例 CSV 有 %d 个点，内置点表 %d 个", len(got), len(want))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("示例 CSV 与内置点表不一致：\n%+v\n%+v", got[i], want[i])
		}
	}
	// Excel 另存的 GBK、带 BOM 的 UTF-8、3x 地址、类型别名、列顺序随意
	gbk := []byte{0xB5, 0xD8, 0xD6, 0xB7, ',', 0xC3, 0xFB, 0xB3, 0xC6, ',', 0xC0, 0xE0, 0xD0, 0xCD, '\n'} // 地址,名称,类型
	gbk = append(gbk, []byte("30001,")...)
	gbk = append(gbk, 0xB5, 0xE7, 0xD1, 0xB9) // 电压
	gbk = append(gbk, []byte(",real\n")...)
	ps, err = parsePointsCSV(gbk)
	if err != nil || len(ps) != 1 || ps[0].Name != "电压" || ps[0].Area != modbus.AreaInputRegisters || ps[0].Type != modbus.TypeFloat32 || ps[0].Order != modbus.OrderABCD {
		t.Errorf("GBK 点表：%+v %v", ps, err)
	}
	ps, err = parsePointsCSV([]byte("\xEF\xBB\xBF类型,名称,地址,枚举\nWORD,状态,346,0：关；1：开\n"))
	if err != nil || len(ps) != 1 || ps[0].Offset != 346 || ps[0].Enum[1] != "开" {
		t.Errorf("BOM 点表：%+v %v", ps, err)
	}
	for _, bad := range []struct{ csv, want string }{
		{"地址,名称\n40001,a\n", "缺少“类型”"},
		{"地址,名称,类型\n40001,a,BCD\n", "第 2 行：类型"},
		{"地址,名称,类型\n40001,a,FLOAT32\n40002,b,INT16\n", "第 3 行"},
		{"地址,名称,类型\n40002,b,INT16\n40001,a,FLOAT32\n", "占用 2 个寄存器，其中 40002"},
		{"地址,名称,类型,字节序\n40001,a,INT16,CDAB\n", "不能用字节序"},
		{"地址,名称,类型,最小,最大\n40001,a,INT16,9,1\n", "大于最大"},
		{"地址,名称,类型\n4x65536,a,FLOAT32\n", "超出协议地址范围"},
	} {
		if _, err := parsePointsCSV([]byte(bad.csv)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q：错误 %v，期望含“%s”", bad.csv, err, bad.want)
		}
	}
}

func TestImportFourAreasAndBool(t *testing.T) {
	data := []byte("地址,名称,类型,读写\n0x0001,启动线圈,BOOL,RW\n1x0001,故障输入,BOOL,R\n30001,使能状态,BOOL,R\n40001,运行命令,BOOL,RW\n")
	ps, err := parsePointsCSV(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		area modbus.Area
		off  uint16
		rw   bool
	}{
		{modbus.AreaCoils, 0, true},
		{modbus.AreaDiscreteInputs, 0, false},
		{modbus.AreaInputRegisters, 0, false},
		{modbus.AreaHoldingRegisters, 0, true},
	}
	if len(ps) != len(want) {
		t.Fatalf("导入 %d 个点，期望 %d：%+v", len(ps), len(want), ps)
	}
	for i, w := range want {
		if ps[i].Area != w.area || ps[i].Offset != w.off || ps[i].Type != modbus.DataType("BOOL") || ps[i].RW != w.rw {
			t.Errorf("第 %d 个点 %+v，期望 %+v", i, ps[i], w)
		}
	}
	imp, err := parsePointsFile("attrs.csv", []byte("属性标识,属性名称,读写模式\n0x0001:BOOL,启动线圈,2\n1x0001:BOOL,故障输入,1\n"))
	if err != nil || len(imp.points) != 2 || len(imp.skipped) != 0 || imp.points[0].Area != modbus.AreaCoils || imp.points[1].Area != modbus.AreaDiscreteInputs {
		t.Fatalf("设备属性表里的 BOOL 位点应保留：%+v %v", imp, err)
	}
}

func TestBitPointColumnsAndWritePermission(t *testing.T) {
	app := test.NewTempApp(t)
	ws := openWS(t, app, false)
	locked(func() {
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaCoils, Offset: 0, Name: "启动线圈", Type: modbus.TypeBool, Order: modbus.OrderAB, RW: false, Scale: 1},
			{Area: modbus.AreaDiscreteInputs, Offset: 0, Name: "故障输入", Type: modbus.TypeBool, Order: modbus.OrderAB, RW: true, Scale: 1},
		}))
		for _, area := range []modbus.Area{modbus.AreaCoils, modbus.AreaDiscreteInputs} {
			d := defaultDef()
			d.Function, d.Kind, d.Qty = area.ReadFunction(), kindPoint, 1
			w := ws.addWindow(d)
			if len(w.cols) != 4 || w.cols[1] != colName {
				t.Errorf("%s 点表窗口应显示名称和单位列：%v", area.Prefix(), w.cols)
			}
			w.mu.Lock()
			w.regs = []uint16{1}
			w.mu.Unlock()
			if text, _ := w.valueText(0); text != "1" {
				t.Errorf("%s BOOL 值 = %q，期望 1", area.Prefix(), text)
			}
			w.sel = 0
			if detail := rowsText(registerInsight(w)); !strings.Contains(detail, ws.points[ptKey{area, 0}].Name) {
				t.Errorf("%s 位点解析应显示点表名称：%s", area.Prefix(), detail)
			}
			ws.session = &session{}
			if w.canWrite() {
				t.Errorf("%s 的只读点不能写", area.Prefix())
			}
			ws.session = nil
		}
	})
}

func TestStringPoints(t *testing.T) {
	ps, err := parsePointsCSV([]byte("地址,名称,类型,长度,字节序\n40901,型号,STRING,7,\n40905,序列号,STRING,4,BA\n40907,温度,FLOAT32,,CDAB\n"))
	if err != nil {
		t.Fatal(err)
	}
	pts := newPointTable(ps)
	h := modbus.AreaHoldingRegisters
	for off, want := range map[uint16]bool{900: false, 901: true, 903: true, 904: false, 905: true, 906: false, 907: true} {
		if got := pts.occupied(h, off); got != want {
			t.Errorf("Offset %d occupied = %v，期望 %v", off, got, want)
		}
	}
	if s := decodeString(modbus.OrderAB, modbus.BytesToRegisters([]byte("HS-1000\x00"))); s != "HS-1000" {
		t.Errorf("AB 字符串 %q", s)
	}
	if s := decodeString(modbus.OrderBA, modbus.BytesToRegisters([]byte("BA21"))); s != "AB12" {
		t.Errorf("BA 字符串 %q", s)
	}
	for _, bad := range []struct{ csv, want string }{
		{"地址,名称,类型\n40001,a,STRING\n", "长度"},
		{"地址,名称,类型,长度\n40001,a,STRING,8\n40003,b,INT16,\n", "第 3 行：地址 40003 与前面的点重复，或被前面的多寄存器点"},
		{"地址,名称,类型,长度\n40003,b,INT16,\n40001,a,STRING,8\n", "占用 4 个寄存器，其中 40003"},
		{"地址,名称,类型,长度,字节序\n40001,a,STRING,8,CDAB\n", "只能是 AB"},
	} {
		if _, err := parsePointsCSV([]byte(bad.csv)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q：错误 %v，期望含“%s”", bad.csv, err, bad.want)
		}
	}
	// 读取窗口按点表显示字符串，后面几个寄存器显示“—”，字符串点不能写
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var got []string
	locked(func() {
		ws.setPoints(pts)
		d := defaultDef()
		d.Start, d.Qty, d.Kind = 900, 6, kindPoint
		w := ws.addWindow(d)
		w.mu.Lock()
		w.regs = append(modbus.BytesToRegisters([]byte("HS-1000\x00")), modbus.BytesToRegisters([]byte("BA21"))...)
		w.mu.Unlock()
		for i := 0; i < 6; i++ {
			v, _ := w.valueText(i)
			got = append(got, v)
		}
		ws.session = &session{}
		w.sel = 0
		if w.canWrite() {
			t.Error("字符串点应只读")
		}
		ws.session = nil
	})
	if strings.Join(got, "|") != "HS-1000|—|—|—|AB12|—" {
		t.Errorf("字符串点显示 %v", got)
	}
	if formatReg(kindASCII, 0x4142) != "AB" || formatReg(kindASCII, 0x4100) != "A·" {
		t.Errorf("ASCII 显示 %q %q", formatReg(kindASCII, 0x4142), formatReg(kindASCII, 0x4100))
	}
	if v, err := parseValue(kindASCII, "A"); err != nil || v != 0x4100 {
		t.Errorf("ASCII 写入 %v %v", v, err)
	}
}

// 64 位：点表里的 64 位点、UINT64 / FLOAT64 显示格式都按寄存器精确显示，超过 2^53 也不丢位；
// 字节序可以按 32 位的写法填，换算成同一种 64 位写法。
func Test64BitPoints(t *testing.T) {
	ps, err := parsePointsCSV([]byte("地址,名称,类型,字节序,单位\n40701,累计电能,ULINT,CDAB,kWh\n40705,累计热量,DOUBLE,,GJ\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].Type != modbus.TypeUint64 || ps[0].Order != modbus.OrderGHEFCDAB || ps[1].Type != modbus.TypeFloat64 || ps[1].Order != modbus.OrderABCDEFGH {
		t.Fatalf("解析得到 %+v", ps)
	}
	if _, err := parsePointsCSV([]byte("地址,名称,类型\n40701,a,INT64\n40703,b,INT16\n")); err == nil || !strings.Contains(err.Error(), "占用") {
		t.Errorf("64 位点占 4 个寄存器，40703 应冲突：%v", err)
	}

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var raw, pts *readWindow
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		ws.setPoints(newPointTable(ps))
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order = 700, 8, kindUint64, modbus.OrderCDAB
		raw = ws.addWindow(d)
		d.Kind = kindPoint
		pts = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "连接并读到数据", func() bool { return hasData(raw) })
	u64, _ := modbus.ParseRaw(modbus.TypeUint64, modbus.OrderGHEFCDAB, "72623859790382857")
	f64, _ := modbus.EncodeRaw(modbus.TypeFloat64, modbus.OrderABCDEFGH, 1234.5)
	var c *modbus.Client
	locked(func() { c = ws.session.client })
	if _, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncWriteMultipleRegisters, Address: 700, Values: append(u64, f64...)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "64 位值精确显示", func() bool {
		r0, _ := raw.valueText(0)
		r1, _ := raw.valueText(1)
		p0, _ := pts.valueText(0)
		p4, _ := pts.valueText(4)
		p5, _ := pts.valueText(5)
		return r0 == "72623859790382857" && r1 == "—" && p0 == "72623859790382857" && p4 == "1234.5" && p5 == "—"
	})
	var text string
	locked(func() {
		if f := raw.def.format(); f != "UINT64 GHEFCDAB" {
			t.Errorf("标题格式 %q", f)
		}
		raw.sel = 2 // 选中 64 位值中间的寄存器，应落回第一个
		raw.sel = raw.align(raw.sel)
		ws.inspect.showRegister(raw)
		text = ws.inspect.text
	})
	if !strings.Contains(text, "INT64 72623859790382857 · UINT64 72623859790382857 · 当前") {
		t.Errorf("检查器缺少 64 位解读：\n%s", text)
	}
	snapshotPNG(t, ws.win, "modbus-ai-64bit.png")
	tap(ws.connBtn)
}

// makeXLSX 生成最小的 xlsx：第一个工作表是说明，第二个是 rows。文字放共享字符串，数字按 Excel 的写法存成 2.0 这种。
func makeXLSX(t *testing.T, rows [][]string) []byte {
	t.Helper()
	var shared []string
	var sheet strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, r+1)
		for c, v := range row {
			if v == "" {
				continue // 空单元格不写，测试按引用补齐
			}
			ref := fmt.Sprintf("%c%d", 'A'+c, r+1)
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				fmt.Fprintf(&sheet, `<c r="%s"><v>%s</v></c>`, ref, strconv.FormatFloat(f, 'f', 1, 64))
				continue
			}
			fmt.Fprintf(&sheet, `<c r="%s" t="s"><v>%d</v></c>`, ref, len(shared))
			shared = append(shared, v)
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	var sst strings.Builder
	sst.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, s := range shared {
		fmt.Fprintf(&sst, `<si><t>%s</t></si>`, html.EscapeString(s))
	}
	sst.WriteString(`</sst>`)
	files := map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="说明" sheetId="1" r:id="rId3"/><sheet name="设备类属性信息" sheetId="2" r:id="rId4"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId3" Target="worksheets/sheet1.xml"/><Relationship Id="rId4" Target="worksheets/sheet2.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
			`<row r="1"><c r="A1" t="inlineStr"><is><t>导出说明：数据类型说明，整数(1)，浮点数(2)</t></is></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": sheet.String(),
		"xl/sharedStrings.xml":     sst.String(),
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 物联网平台（Telegraf 采集）导出的设备属性表：地址和类型写在属性标识里，没有字节序；
// 虚拟点位、位区里的多寄存器类型等表示不了的行跳过并说明，其他照常导入。
func TestImportAttrTable(t *testing.T) {
	head := []string{"属性ID", "属性标识", "属性名称", "标准化名称", "读写模式", "单位", "计算公式", "数据类型", "排列顺序", "模式"}
	rows := [][]string{head,
		{"198617", "4x0001:REAL", "一次供温", "m_001t", "1", "℃", "", "2", "0", "0"},
		{"198618", "4x0003:REAL", "一次回温", "m_002t", "1", "℃", "", "2", "1", "0"},
		{"198619", "4x0005:INT", "补水泵频率", "", "2", "Hz", "x*0.01", "1", "2", "0"},
		{"198620", "4x0006", "温差", "", "1", "℃", "/10", "1", "3", "0"},
		{"198621", "", "日均温度", "", "1", "℃", "", "2", "4", "1"},
		{"198622", "4x0007:BOOL", "报警", "", "1", "", "", "6", "5", "0"},
		{"198623", "0x0001:REAL", "线圈", "", "1", "", "", "2", "6", "0"},
		{"198624", "4x0002:REAL", "重叠", "", "1", "", "", "2", "7", "0"},
		{"198625", "3x0011:DOUBLE", "累计热量", "", "1", "GJ", "x*0.1+5", "3", "8", "0"},
		{"198626", "1x0002", "故障输入", "", "1", "", "", "6", "9", "0"},
	}
	imp, err := parsePointsFile("热力站A型属性列表.xlsx", makeXLSX(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		area  modbus.Area
		off   uint16
		name  string
		typ   modbus.DataType
		order modbus.ByteOrder
		scale float64
		rw    bool
	}
	wants := []want{
		{modbus.AreaHoldingRegisters, 0, "一次供温", modbus.TypeFloat32, modbus.OrderABCD, 1, false},
		{modbus.AreaHoldingRegisters, 2, "一次回温", modbus.TypeFloat32, modbus.OrderABCD, 1, false},
		{modbus.AreaHoldingRegisters, 4, "补水泵频率", modbus.TypeInt16, modbus.OrderAB, 0.01, true},
		{modbus.AreaHoldingRegisters, 5, "温差", modbus.TypeInt16, modbus.OrderAB, 0.1, false},
		{modbus.AreaHoldingRegisters, 6, "报警", modbus.TypeBool, modbus.OrderAB, 1, false},
		{modbus.AreaInputRegisters, 10, "累计热量", modbus.TypeFloat64, modbus.OrderABCDEFGH, 1, false},
		{modbus.AreaDiscreteInputs, 1, "故障输入", modbus.TypeBool, modbus.OrderAB, 1, false},
	}
	if imp.format != "设备属性表" || !imp.noOrder || len(imp.points) != len(wants) {
		t.Fatalf("得到 %s %v %d 个点：%+v\n跳过：%v", imp.format, imp.noOrder, len(imp.points), imp.points, imp.skipped)
	}
	for i, w := range wants {
		p := imp.points[i]
		if p.Area != w.area || p.Offset != w.off || p.Name != w.name || p.Type != w.typ || p.Order != w.order || p.Scale != w.scale || p.RW != w.rw {
			t.Errorf("第 %d 个点 %+v，期望 %+v", i, p, w)
		}
	}
	skipped := strings.Join(imp.skipped, "\n")
	for _, s := range []string{"日均温度：虚拟点位", "线圈：0x 位区只能", "重叠：地址 40002"} {
		if !strings.Contains(skipped, s) {
			t.Errorf("跳过说明缺少“%s”：\n%s", s, skipped)
		}
	}
	if len(imp.notes) != 1 || !strings.Contains(imp.notes[0], "x*0.1+5") {
		t.Errorf("计算公式提示 %v", imp.notes)
	}

	// 属性标识带字节序时按它来；另存成 CSV 的属性表同样能识别
	csvData := "属性标识,属性名称,数据类型\n4x0001:REAL:CDAB,一次供温,2\n"
	imp, err = parsePointsFile("a.csv", []byte(csvData))
	if err != nil || imp.noOrder || imp.points[0].Order != modbus.OrderCDAB {
		t.Fatalf("CSV 属性表：%+v %v", imp, err)
	}
	// 本程序格式的点表存成 xlsx 也能导入；数字单元格存成 40005.0 这种写法时按整数识别
	imp, err = parsePointsFile("p.xlsx", makeXLSX(t, [][]string{{"地址", "名称", "类型", "倍率", "长度"},
		{"40005", "二次温差", "INT16", "0.1", ""}, {"40901", "设备型号", "STRING", "", "16"}}))
	if err != nil || imp.format != "点表" || len(imp.points) != 2 || imp.points[0].Offset != 4 || imp.points[0].Scale != 0.1 || imp.points[1].Len != 16 {
		t.Fatalf("xlsx 点表：%+v %v", imp, err)
	}
	if _, err := parsePointsFile("a.xlsx", makeXLSX(t, [][]string{{"甲", "乙"}, {"1", "2"}})); err == nil || !strings.Contains(err.Error(), "没有找到点表") {
		t.Errorf("认不出的 xlsx 应报错：%v", err)
	}
	if _, err := parsePointsFile("a.xlsx", []byte("not zip")); err == nil {
		t.Error("坏文件应报错")
	}
}

// 按点表新建读取窗口：同一数据区里相近的点合成一个窗口，相隔太远或超过 120 个寄存器另开。
func TestDefsForPoints(t *testing.T) {
	ps := []point{
		{Area: modbus.AreaHoldingRegisters, Offset: 20, Type: modbus.TypeFloat32}, // 与 40001 相隔 18 个寄存器，合并
		{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeFloat32},
		{Area: modbus.AreaHoldingRegisters, Offset: 300, Type: modbus.TypeInt16},
		{Area: modbus.AreaHoldingRegisters, Offset: 400, Type: modbus.TypeInt16},
		{Area: modbus.AreaHoldingRegisters, Offset: 418, Type: modbus.TypeInt16},
		{Area: modbus.AreaHoldingRegisters, Offset: 530, Type: modbus.TypeUint64},
		{Area: modbus.AreaInputRegisters, Offset: 0, Type: modbus.TypeInt16},
	}
	var got []string
	for _, d := range defsForPoints(ps) {
		got = append(got, fmt.Sprintf("%02X %s", byte(d.Function), refSpan(d.area(), d.Start, d.Qty)))
	}
	want := []string{"04 30001", "03 40001–40022", "03 40301", "03 40401–40419", "03 40531–40534"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("得到 %v，期望 %v", got, want)
	}
}

func TestDefsForPointsKeepsAllSparseAreas(t *testing.T) {
	ps := []point{
		{Area: modbus.AreaCoils, Offset: 0, Type: modbus.TypeBool},
		{Area: modbus.AreaDiscreteInputs, Offset: 0, Type: modbus.TypeBool},
		{Area: modbus.AreaInputRegisters, Offset: 0, Type: modbus.TypeInt16},
	}
	for i := range 9 {
		ps = append(ps, point{Area: modbus.AreaHoldingRegisters, Offset: uint16(i * 100), Type: modbus.TypeInt16})
	}
	defs := defsForPoints(ps)
	if len(defs) != len(ps) {
		t.Fatalf("%d 个分散点应全部建窗，得到 %d 个：%+v", len(ps), len(defs), defs)
	}
	for i, want := range []modbus.FunctionCode{modbus.FuncReadCoils, modbus.FuncReadDiscreteInputs, modbus.FuncReadInputRegisters, modbus.FuncReadHoldingRegisters} {
		if defs[i].Function != want || defs[i].Kind != kindPoint {
			t.Errorf("第 %d 个窗口 %+v，期望功能码 %02X 且按点表显示", i, defs[i], byte(want))
		}
	}
}

func TestImportAddsWindowsForUncoveredPoints(t *testing.T) {
	app := test.NewTempApp(t)
	ws := openWS(t, app, false)
	locked(func() {
		covered := defaultDef()
		covered.Start, covered.Qty = 0, 2
		ws.addWindow(covered)
		partial := defaultDef()
		partial.Start, partial.Qty = 500, 1
		ws.addWindow(partial)
		imp := pointImport{format: "点表", points: []point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeInt16, Name: "已覆盖"},
			{Area: modbus.AreaHoldingRegisters, Offset: 500, Type: modbus.TypeFloat32, Name: "跨界"},
			{Area: modbus.AreaDiscreteInputs, Offset: 0, Type: modbus.TypeBool, Name: "故障"},
		}}
		ws.applyImport(imp)
		if len(ws.windows) != 4 {
			t.Fatalf("已有两窗，只应为跨界点和离散输入补两窗：%+v", ws.windows)
		}
		for _, p := range imp.points {
			found := false
			for _, w := range ws.windows {
				d := w.def
				if d.Kind == kindPoint && d.area() == p.Area && int(d.Start) <= int(p.Offset) && int(p.Offset)+p.regs() <= int(d.Start)+d.Qty {
					found = true
				}
			}
			if !found {
				t.Errorf("点 %s 没有被完整覆盖", p.Name)
			}
		}
	})
}

// 点表里的浮点数按 ABCD 解出来不合理、按 CDAB 全部合理：读取窗口建议改字节序，一键改点表。
func TestPointOrderSuggestion(t *testing.T) {
	imp, err := parsePointsFile("a.csv", []byte("属性标识,属性名称\n4x0701:REAL,一次供温\n4x0703:REAL,一次回温\n4x0705:INT,状态\n4x0706:DINT,累计\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		msg := ws.applyImport(imp)
		if !strings.Contains(msg, "新建了读取窗口：40701–40707") || !strings.Contains(msg, "没有字节序") {
			t.Errorf("导入说明：%s", msg)
		}
		w = ws.windows[0]
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "连接并读到数据", func() bool { return hasData(w) })
	f1, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 85.5)
	f2, _ := modbus.EncodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, 60.25)
	n, _ := modbus.EncodeRaw(modbus.TypeInt32, modbus.OrderCDAB, 123456)
	var c *modbus.Client
	locked(func() { c = ws.session.client })
	vals := append(append(append(f1, f2...), 1), n...)
	if _, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncWriteMultipleRegisters, Address: 700, Values: vals}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "建议点表改用 CDAB", func() bool { return w.actionBtn.Visible() && w.actionBtn.Text == "点表改用 CDAB" })
	tap(w.actionBtn)
	waitFor(t, 5*time.Second, "改字节序后显示正确的值", func() bool {
		v0, _ := w.valueText(0)
		v2, _ := w.valueText(2)
		v5, _ := w.valueText(5)
		return v0 == "85.5" && v2 == "60.25" && v5 == "123456" && !w.actionBtn.Visible()
	})
	locked(func() {
		if p, _ := ws.points.get(modbus.AreaHoldingRegisters, 704); p.Order != modbus.OrderAB {
			t.Errorf("16 位点不应改字节序：%s", p.Order)
		}
	})
	tap(ws.connBtn)
}

func TestChangePointOrderByScope(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 10, Type: modbus.TypeFloat32, Order: modbus.OrderCDAB, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 20, Type: modbus.TypeUint64, Order: modbus.OrderGHEFCDAB, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 30, Type: modbus.TypeInt16, Order: modbus.OrderAB, Scale: 1},
			{Area: modbus.AreaInputRegisters, Offset: 0, Type: modbus.TypeFloat32, Order: modbus.OrderBADC, Scale: 1},
		}))
		d := defaultDef()
		d.Start, d.Qty, d.Kind = 0, 31, kindPoint
		w := ws.addWindow(d)
		w.mu.Lock()
		w.regs = make([]uint16, 31)
		w.mu.Unlock()
		d.Function = modbus.FuncReadInputRegisters
		ws.addWindow(d)
		order := func(area modbus.Area, off uint16) modbus.ByteOrder { return ws.points[ptKey{area, off}].Order }
		h, in := modbus.AreaHoldingRegisters, modbus.AreaInputRegisters
		if n := ws.changePointOrder(orderOne, w, 10, modbus.OrderDCBA); n != 1 || order(h, 10) != modbus.OrderDCBA || order(h, 0) != modbus.OrderABCD {
			t.Errorf("单点修改影响了其他点：%d, %v", n, ws.points)
		}
		if n := ws.changePointOrder(orderWindow, w, 0, modbus.OrderBADC); n != 3 || order(h, 0) != modbus.OrderBADC || order(h, 10) != modbus.OrderBADC || order(h, 20) != modbus.OrderBADCFEHG || order(h, 30) != modbus.OrderAB || order(in, 0) != modbus.OrderBADC {
			t.Errorf("当前窗口修改范围或 64 位映射错误：%d, %v", n, ws.points)
		}
		if n := ws.changePointOrder(orderAll, nil, 0, modbus.OrderABCD); n != 4 || order(h, 20) != modbus.OrderABCDEFGH || order(in, 0) != modbus.OrderABCD || order(h, 30) != modbus.OrderAB {
			t.Errorf("全部点修改范围错误：%d, %v", n, ws.points)
		}
		if regs, _, _ := w.snapshot(); len(regs) != 31 {
			t.Errorf("调整字节序不应清空正在显示的读数，剩余 %d 个", len(regs))
		}
	})
}

func TestInspectorShowsAdjacentFloatReadingsWithoutChoosing(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		p := point{Area: modbus.AreaHoldingRegisters, Offset: 1, Name: "温度", Type: modbus.TypeFloat32, Order: modbus.OrderCDAB, Scale: 1}
		ws.setPoints(newPointTable([]point{p}))
		d := defaultDef()
		d.Start, d.Qty, d.Kind = 0, 4, kindPoint
		w := ws.addWindow(d)
		w.mu.Lock()
		w.regs = []uint16{0x4170, 0x0000, 0x4171, 0x0000}
		w.mu.Unlock()
		w.sel = 1
		text := rowsText(registerInsight(w))
		for _, want := range []string{"-1 ABCD", "+1 ABCD", "15.0（合理）", "15.0625（合理）", "对照设备面板", "不会自动"} {
			if !strings.Contains(text, want) {
				t.Errorf("邻址解读缺少 %q：\n%s", want, text)
			}
		}
		if got := ws.points[ptKey{p.Area, p.Offset}].Order; got != modbus.OrderCDAB {
			t.Errorf("解析不应自动改字节序，得到 %s", got)
		}
		w.sel = 0
		if text := rowsText(registerInsight(w)); strings.Contains(text, "地址 -1 ·") {
			t.Errorf("窗口起点没有前一个寄存器，不应展示前移候选：%s", text)
		}
	})
}

// parsePointsCSV 按本程序的格式解析 CSV 点表（不识别设备属性表）。
func parsePointsCSV(data []byte) ([]point, error) {
	rows, err := readCSV(data)
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, errors.New("点表至少要有表头和一行数据")
	}
	return parsePointRows(rows)
}
