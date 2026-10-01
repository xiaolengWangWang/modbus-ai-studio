package ui

import (
	"os"
	"reflect"
	"strings"
	"testing"

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
		{"地址,名称,类型\n40001,a,DOUBLE\n", "第 2 行：类型"},
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
