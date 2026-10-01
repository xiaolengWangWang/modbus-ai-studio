package ui

import (
	"context"
	"os"
	"reflect"
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
		{"地址,名称,类型\n10001,a,INT16\n", "1x 区"},
		{"地址,名称,类型,字节序\n40001,a,INT16,CDAB\n", "不能用字节序"},
		{"地址,名称,类型,最小,最大\n40001,a,INT16,9,1\n", "大于最大"},
	} {
		if _, err := parsePointsCSV([]byte(bad.csv)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q：错误 %v，期望含“%s”", bad.csv, err, bad.want)
		}
	}
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
