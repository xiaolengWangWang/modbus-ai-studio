package ui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

// locked 在 uiMu 下执行，和后台 goroutine 发起的界面更新互斥。
func locked(fn func()) {
	uiMu.Lock()
	defer uiMu.Unlock()
	fn()
}

func tap(b *widget.Button) { locked(func() { test.Tap(b) }) }

func closeWS(ws *Workspace) {
	locked(func() {
		if !ws.closed {
			ws.win.Close()
		}
	})
}

// openWS 在锁内打开主窗口，测试结束时关闭，停止它的后台刷新。
func openWS(t *testing.T, a fyne.App, demo bool) *Workspace {
	var ws *Workspace
	locked(func() {
		ws = open(a, "test", int(winSeq.Add(1)))
		if demo {
			ws.loadDemo()
		}
	})
	t.Cleanup(func() { closeWS(ws) })
	return ws
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		var done bool
		locked(func() { done = ok() })
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v 内没有等到：%s", d, what)
		}
	}
}

func hasData(w *readWindow) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.regs != nil && w.err == nil
}

func snapshotPNG(t *testing.T, w fyne.Window, name string) {
	dir := os.Getenv("MODBUS_AI_SNAPSHOT")
	if dir == "" {
		return
	}
	time.Sleep(300 * time.Millisecond)
	var img image.Image
	locked(func() { img = w.Canvas().Capture() })
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// 用 Fyne 软件渲染在无界面环境跑一遍：连接内置模拟器，等数据到达，检查表格内容；
// 窗口 3 的地址段应答慢于超时，应自动分析出“晚到响应”，一键把超时调大后恢复正常。
// 设置 MODBUS_AI_SNAPSHOT=<目录> 时保存截图，供布局检查（设计文档 13.10）。
func TestUIWithBuiltinSimulator(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, true) // 打开换热站示例时自动连接内置模拟器，不用点“连接”
	w1, w2, w3 := ws.windows[0], ws.windows[1], ws.windows[2]
	waitFor(t, 6*time.Second, "窗口 1 收到数据、窗口 3 超时", func() bool {
		w3.mu.Lock()
		slow := w3.errN > 0
		w3.mu.Unlock()
		return hasData(w1) && hasData(w2) && slow
	})

	checks := []struct {
		w    *readWindow
		i    int
		want string
	}{
		{w1, 0, "45.2"},   // 二次供水温度 FLOAT32 CDAB
		{w1, 1, "—"},      // 32 位点的第二个寄存器
		{w1, 4, "13.2"},   // 二次温差 INT16 × 0.1
		{w1, 5, "0x0001"}, // 状态字
		{w1, 14, "42.50"}, // 循环泵频率 × 0.01
		{w1, 18, "0 停止"},  // 补水泵状态（枚举含义）
		{w2, 0, "15.0"},   // 温差设定 40347
		{w2, 6, "1 自动"},   // 运行模式 40353
	}
	for _, c := range checks {
		var got string
		locked(func() { got, _ = c.w.valueText(c.i) })
		if got != c.want {
			t.Errorf("窗口 %d 第 %d 个寄存器：显示 %q，期望 %q", c.w.no, c.i, got, c.want)
		}
	}

	// 晚到响应在保护间隔内到达，诊断应识别出“设备有应答但慢”，并给出一键调大超时
	waitFor(t, 8*time.Second, "窗口 3 的诊断给出调大超时", func() bool {
		return w3.errLbl.Visible() && strings.HasPrefix(w3.actionBtn.Text, "超时改为") && w3.actionBtn.Visible()
	})
	var hint string
	locked(func() { hint = w3.hintLbl.Text })
	if !strings.Contains(hint, "设备有应答") {
		t.Errorf("诊断说明：%q", hint)
	}
	snapshotPNG(t, ws.win, "modbus-ai-ui.png")
	tap(w3.actionBtn)
	locked(func() {
		if ws.timeout != 2*time.Second || ws.session.client.Timeout() != 2*time.Second {
			t.Errorf("超时应改为 2000 ms，界面 %v，连接 %v", ws.timeout, ws.session.client.Timeout())
		}
		// 窗口 3 扫描周期 5 s，缩短后立即重新轮询，不必等下一轮
		ws.redefine(w3, func(d *readDef) { d.Scan = 300 * time.Millisecond })
	})
	waitFor(t, 5*time.Second, "窗口 3 恢复正常", func() bool { return hasData(w3) && !w3.errLbl.Visible() })

	// 单击选中值：解析面板给出多解释视图
	locked(func() { w2.table.Select(widget.TableCellID{Row: 1, Col: 2}) })
	var text string
	locked(func() { text = ws.inspect.text })
	snapshotPNG(t, ws.win, "modbus-ai-inspect.png")
	if w2.sel != 0 || !strings.Contains(text, "40347") || !strings.Contains(text, "FLOAT32 15.0（合理）") || !strings.Contains(text, "温差设定") {
		t.Errorf("选中第二个寄存器应对齐到 40347，解析：\n%s", text)
	}

	// 通信报文：选中一行自动暂停并解析
	locked(func() {
		ws.traffic.flush()
		ws.traffic.list.Select(0)
	})
	locked(func() { text = ws.inspect.text })
	if !ws.traffic.paused || !strings.Contains(text, "报文解析") || !strings.Contains(text, "Slave ID") {
		t.Errorf("选中报文后应暂停并解析，paused=%v：\n%s", ws.traffic.paused, text)
	}
	snapshotPNG(t, ws.win, "modbus-ai-packet.png")
	locked(func() { ws.traffic.setPaused(false) })
	tap(ws.connBtn) // 断开，停止轮询和内置模拟器
}

// 读取定义换成 FLOAT32 ABCD 读温差设定：值不合理，自动建议改用 CDAB，一键修正。
func TestFloatOrderSuggestion(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order = 346, 4, kindFloat32, modbus.OrderABCD
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "建议改用 CDAB", func() bool { return hasData(w) && w.actionBtn.Text == "改用 CDAB" && w.actionBtn.Visible() })
	tap(w.actionBtn)
	waitFor(t, 5*time.Second, "改用 CDAB 后显示 15.0", func() bool {
		v, _ := w.valueText(0)
		return w.def.Order == modbus.OrderCDAB && v == "15.0"
	})
	tap(ws.connBtn)
}

// 读取范围越界：异常 02 给出原因，并提供逐个探测。
func TestIllegalAddressDiagnosis(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		d := defaultDef()
		d.Start = 995 // 模拟器只有 0–999
		w = ws.addWindow(d)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "异常 02 诊断", func() bool {
		return strings.Contains(w.errLbl.Text, "异常 02") && w.actionBtn.Text == "逐个探测可读地址"
	})
	var hint string
	var s *session
	locked(func() { hint, s = w.hintLbl.Text, ws.session })
	if !strings.Contains(hint, "40996–41005") {
		t.Errorf("诊断应说明读取范围：%q", hint)
	}
	res, err := probe(context.Background(), s.client, w.def, func(int) {})
	if err != nil || fmt.Sprint(res) != "[1 1 1 1 1 -1 -1 -1 -1 -1]" {
		t.Errorf("探测结果 %v %v", res, err)
	}
	tap(ws.connBtn)
}

// 协议选错：设备是 RTU over TCP，却按 Modbus TCP 连接。诊断给出“识别协议”，一键识别后自动改协议并重新连接。
func TestDetectProtocolFromDiagnosis(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() { w = ws.addWindow(defaultDef()) })
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.target.SetText(ln.Addr().String())
		ws.proto.SetSelected(protoTCP)
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "诊断给出识别协议", func() bool { return w.actionBtn.Text == "识别协议" && w.actionBtn.Visible() })
	tap(w.actionBtn)
	waitFor(t, 10*time.Second, "识别为 RTU over TCP 并重新连接", func() bool {
		return ws.proto.Selected == protoRTUTCP && ws.session != nil && hasData(w)
	})
}

// 多开：两个主窗口各自连接内置模拟器，互不影响；断开一个，另一个继续轮询。
func TestMultipleWindows(t *testing.T) {
	a := test.NewTempApp(t)
	one := openWS(t, a, true)
	var two *Workspace
	locked(func() {
		one.proto.SetSelected(protoTCP)
		two = one.openNew()
	})
	t.Cleanup(func() { closeWS(two) })
	if two.proto.Selected != protoTCP || !two.useSim.Checked {
		t.Errorf("新窗口应沿用协议和内置模拟器设置：%s %v", two.proto.Selected, two.useSim.Checked)
	}
	if one.no == two.no || !strings.Contains(two.win.Title(), "窗口") {
		t.Fatalf("窗口编号 %d / %d，标题 %q", one.no, two.no, two.win.Title())
	}
	if len(two.windows) != 0 || two.session != nil {
		t.Errorf("新主窗口应为空且未连接：%d 个读取窗口，session=%v", len(two.windows), two.session)
	}
	locked(func() { two.addWindow(defaultDef()) })
	tap(two.connBtn) // 窗口 1 是示例窗口，已自动连接
	waitFor(t, 5*time.Second, "两个窗口都有数据", func() bool { return hasData(one.windows[0]) && hasData(two.windows[0]) })
	tap(one.connBtn)
	var before int
	two.windows[0].mu.Lock()
	before = two.windows[0].tx
	two.windows[0].mu.Unlock()
	waitFor(t, 3*time.Second, "窗口 2 继续轮询", func() bool {
		two.windows[0].mu.Lock()
		defer two.windows[0].mu.Unlock()
		return two.windows[0].tx > before+1 && one.session == nil
	})
	locked(func() { two.win.Close() })
	if !two.closed || two.session != nil {
		t.Error("关闭主窗口应断开它的连接")
	}
}

// 连接栏按 1024 宽的工控机屏幕设计：两种协议下的最小宽度都不能超过 1024。
func TestConnectionBarFits1024(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	for _, p := range protoNames {
		var w float32
		locked(func() {
			ws.proto.SetSelected(p)
			w = ws.bar.MinSize().Width
		})
		if w > 1024 {
			t.Errorf("%s 连接栏最小宽度 %.0f，超过 1024", p, w)
		}
		t.Logf("%s 连接栏最小宽度 %.0f", p, w)
	}
	var cfg connConfig
	var err error
	locked(func() {
		ws.port.SetOptions([]string{"COM3"})
		ws.port.SetSelected("COM3")
		ws.frameFmt.SetSelected("8E1")
		ws.baud.SetText("19200")
		cfg, err = ws.connConfig()
	})
	if err != nil || cfg.serial.Parity != "E" || cfg.serial.StopBits != 1 || cfg.serial.DataBits != 8 || cfg.serial.BaudRate != 19200 {
		t.Errorf("串口参数 %+v %v", cfg.serial, err)
	}
}

// 主窗口初始为空：没有读取窗口、不连接、报文为空，只显示新建提示。
func TestInitiallyEmpty(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	time.Sleep(300 * time.Millisecond)
	locked(func() {
		if len(ws.windows) != 0 || ws.session != nil || ws.connecting || ws.traffic.total != 0 {
			t.Errorf("初始应为空：%d 个读取窗口，session=%v，connecting=%v，报文 %d 条", len(ws.windows), ws.session, ws.connecting, ws.traffic.total)
		}
		if len(ws.tiles.Objects) != 1 {
			t.Error("应显示新建读取窗口的提示")
		}
	})
}

func TestSerialPortClaim(t *testing.T) {
	if err := claimPort("COM_TEST", 1); err != nil {
		t.Fatal(err)
	}
	defer releasePort("COM_TEST")
	if err := claimPort("COM_TEST", 2); err == nil || !strings.Contains(err.Error(), "窗口 1") {
		t.Fatalf("同一串口在另一个窗口打开应报错，得到 %v", err)
	}
	if err := claimPort("COM_TEST", 1); err != nil {
		t.Fatalf("同一窗口重复占用应允许：%v", err)
	}
}

func TestDescribePacket(t *testing.T) {
	rx := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeRTUOverTCP, Function: modbus.FuncReadHoldingRegisters, Address: 346, Count: 2,
		Raw: modbus.AppendCRC([]byte{0x01, 0x03, 0x04, 0x00, 0x00, 0x41, 0x70}), Status: modbus.StatusSuccess, RequestID: 7}
	text := rowsText(describePacket(rx, demoPoints()))
	for _, want := range []string{"Slave ID = 1", "03 读保持寄存器", "字节数", "40347", "温差设定 15.0 ℃", "校验正确"} {
		if !strings.Contains(text, want) {
			t.Errorf("RTU 响应解析缺少 %q：\n%s", want, text)
		}
	}
	bad := rx
	bad.Raw = append([]byte(nil), rx.Raw...)
	bad.Raw[len(bad.Raw)-1] ^= 0xFF
	bad.Status = modbus.StatusCRCError
	if text := rowsText(describePacket(bad, demoPoints())); !strings.Contains(text, "校验错误，应为") || !strings.Contains(text, "校验（RTU 为 CRC") {
		t.Errorf("CRC 错误解析：\n%s", text)
	}
	ex := modbus.Packet{Dir: modbus.DirRX, Mode: modbus.ModeRTU, Raw: modbus.AppendCRC([]byte{0x01, 0x83, 0x02}), Status: modbus.StatusException}
	if text := rowsText(describePacket(ex, demoPoints())); !strings.Contains(text, "Illegal Data Address") || !strings.Contains(text, "±1") {
		t.Errorf("异常响应解析：\n%s", text)
	}
	tx := modbus.Packet{Dir: modbus.DirTX, Mode: modbus.ModeTCP, Raw: modbus.EncodeADU(modbus.ModeTCP, 1, 9, []byte{0x10, 0x01, 0x5A, 0x00, 0x02, 0x04, 0x00, 0x00, 0x41, 0x70})}
	text = rowsText(describePacket(tx, demoPoints()))
	for _, want := range []string{"Transaction ID = 9", "Unit ID = 1", "Offset 346（40347）", "40348", "16752"} {
		if !strings.Contains(text, want) {
			t.Errorf("MBAP 写请求解析缺少 %q：\n%s", want, text)
		}
	}
	coils := describePDUOnly(modbus.DirRX, []byte{0x01, 0x00, 0x00, 0x00, 0x0A}, []byte{0x01, 0x02, 0x05, 0x02}, nil)
	if text := rowsText(coils); !strings.Contains(text, "1=1  2=0  3=1") || !strings.Contains(text, "10=1") {
		t.Errorf("线圈响应解析：\n%s", text)
	}
}

func TestValueFormats(t *testing.T) {
	cases := []struct {
		k    valueKind
		in   string
		want float64
	}{
		{kindSigned, "-5", -5}, {kindHex, "015A", 346}, {kindHex, "0x015a", 346}, {kindBinary, "0000 0001 0101 1010", 346},
		{kindUnsigned, "0x10", 16}, {kindFloat32, "15.5", 15.5},
	}
	for _, c := range cases {
		if v, err := parseValue(c.k, c.in); err != nil || v != c.want {
			t.Errorf("parseValue(%s, %q) = %v, %v", c.k, c.in, v, err)
		}
	}
	if _, err := parseValue(kindSigned, "abc"); err == nil {
		t.Error("非数字应报错")
	}
	if got := formatReg(kindBinary, 0x015A); got != "0000 0001 0101 1010" {
		t.Errorf("Binary %q", got)
	}
	if got := formatReg(kindSigned, 0xFFFB); got != "-5" {
		t.Errorf("Signed %q", got)
	}
	if got, ok := formatWide(kindFloat32, modbus.OrderCDAB, []uint16{0x0000, 0x4170}); got != "15.0" || !ok {
		t.Errorf("FLOAT32 CDAB %q %v", got, ok)
	}
	if o, ok := suggestFloatOrder([]uint16{0x0000, 0x4170, 0x0000, 0x4234}, modbus.OrderABCD); !ok || o != modbus.OrderCDAB {
		t.Errorf("字节序建议 %v %v", o, ok)
	}
	if _, ok := suggestFloatOrder([]uint16{0x4170, 0x0000}, modbus.OrderABCD); ok {
		t.Error("值合理时不应给建议")
	}
	if b, err := parseHex("01 03,0x00 005A"); err != nil || hexs(b) != "01 03 00 00 5A" {
		t.Errorf("parseHex %s %v", hexs(b), err)
	}
	if _, err := parseHex("1 03"); err == nil {
		t.Error("奇数位应报错")
	}
	if suggestTimeout(1100) != 2000 || suggestTimeout(100) != 500 {
		t.Errorf("超时建议 %d %d", suggestTimeout(1100), suggestTimeout(100))
	}
	if errSummary(errors.New("x")) != "x" {
		t.Error("errSummary")
	}
}

func TestTile(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	n := len(ws.windows)
	locked(func() {
		for i := 0; i < 4; i++ {
			ws.addWindow(defaultDef())
		}
	})
	if len(ws.windows) != n+4 {
		t.Fatalf("应有 %d 个读取窗口，得到 %d", n+4, len(ws.windows))
	}
	locked(func() {
		for len(ws.windows) > 0 {
			ws.removeWindow(ws.windows[0])
		}
	})
	if len(ws.tiles.Objects) != 1 {
		t.Fatal("没有读取窗口时应显示提示")
	}
	// 已关闭的窗口不能被异步回调重新启动轮询
	var closedWin *readWindow
	locked(func() {
		closedWin = ws.addWindow(defaultDef())
		ws.removeWindow(closedWin)
		ws.session = &session{}
		closedWin.start()
		ws.session = nil
	})
	if closedWin.stop != nil {
		t.Error("已关闭的读取窗口不应开始轮询")
	}
}

// 示例 CSV 既是给用户的模板，也必须与内置换热站点表完全一致。
func TestPointsCSV(t *testing.T) {
	data, err := os.ReadFile("../../examples/heat-station-points.csv")
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

// 工作区保存再打开：连接参数、读取窗口和点表都原样恢复，打开后不自动连接。
func TestWorkspaceRoundTrip(t *testing.T) {
	a := test.NewTempApp(t)
	src := openWS(t, a, true)
	var data []byte
	var err error
	locked(func() {
		src.useSim.SetChecked(false)
		src.target.SetText("192.168.1.20:502")
		src.setTimeout(1500 * time.Millisecond)
		src.windows[1].def.Name = "设定值"
		data, err = src.encodeWorkspace()
	})
	if err != nil {
		t.Fatal(err)
	}
	dst := openWS(t, a, false)
	locked(func() { err = dst.applyWorkspace(data) })
	if err != nil {
		t.Fatal(err)
	}
	locked(func() {
		if dst.session != nil || dst.useSim.Checked || dst.target.Text != "192.168.1.20:502" || dst.timeout != 1500*time.Millisecond || dst.proto.Selected != protoRTUTCP {
			t.Errorf("连接参数没恢复：sim=%v target=%q timeout=%v proto=%s session=%v", dst.useSim.Checked, dst.target.Text, dst.timeout, dst.proto.Selected, dst.session)
		}
		if len(dst.windows) != 3 || dst.windows[1].def != src.windows[1].def || len(dst.points) != len(demoPoints()) {
			t.Errorf("读取窗口或点表没恢复：%d 个窗口，%d 个点", len(dst.windows), len(dst.points))
		}
		if len(dst.windows[0].cols) != 4 {
			t.Error("恢复的点表应让窗口 1 显示名称、单位列")
		}
	})
	locked(func() { err = dst.applyWorkspace([]byte(`{"mode":"X"}`)) })
	if err == nil {
		t.Error("坏文件应报错")
	}
}

func TestScanSlaves(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 5, simulator.HeatStation())
	srv.SetFaults(simulator.Faults{Exceptions: []simulator.ExceptionRange{{AddrRange: simulator.AddrRange{Start: 0, Count: 1}, Code: modbus.ExceptionIllegalDataAddress}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeRTUOverTCP, Guard: 10 * time.Millisecond})
	defer c.Close()
	hits, err := scanSlaves(context.Background(), c, 3, 7, 60*time.Millisecond, func(int, []slaveHit) {})
	if err != nil || hitIDs(hits) != "5" || hits[0].err == nil {
		t.Fatalf("应只找到 Slave 5（异常响应也算在线）：%v %v", hits, err)
	}
}

// 串口参数扫描：设备是 19200 8E1，前面 5 种组合没有应答，第 6 种找到。
func TestScanSerial(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	live, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(live)
	silent, _ := net.Listen("tcp", "127.0.0.1:0") // 接受连接但从不应答，相当于参数不对
	t.Cleanup(func() { srv.Close(); silent.Close() })
	go func() {
		for {
			if _, err := silent.Accept(); err != nil {
				return
			}
		}
	}()
	var tried []string
	open := func(cfg transport.SerialConfig) (modbus.Transport, error) {
		addr := silent.Addr().String()
		if cfg.BaudRate == 19200 && cfg.Parity == "E" && cfg.StopBits == 1 && cfg.DataBits == 8 {
			addr = live.Addr().String()
		}
		return net.Dial("tcp", addr)
	}
	mode, baud, format, err := scanSerial(context.Background(), modbus.ModeRTU, open, "COM9", 1, 80*time.Millisecond, nil,
		func(_, _ int, try string) { tried = append(tried, try) })
	if err != nil || mode != modbus.ModeRTU || baud != 19200 || format != "8E1" || len(tried) != 6 {
		t.Fatalf("找到 %s %d %s（%v），试了 %v", mode, baud, format, err, tried)
	}
}

// ASCII over TCP 全流程：内置模拟器按 ASCII 应答，读取窗口照常显示工程值，报文按字符显示并逐字段解析。
func TestASCIIEndToEnd(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.proto.SetSelected(protoASCIITCP)
		ws.loadDemo()
	})
	waitFor(t, 5*time.Second, "ASCII 模式收到数据", func() bool { return hasData(ws.windows[0]) && hasData(ws.windows[1]) })
	var v1, v2 string
	var rx modbus.Packet
	locked(func() {
		v1, _ = ws.windows[0].valueText(0)
		v2, _ = ws.windows[1].valueText(0)
		ws.traffic.flush()
		for _, p := range ws.traffic.all {
			if p.Dir == modbus.DirRX && p.Status == modbus.StatusSuccess && p.Address == 346 {
				rx = p
			}
		}
	})
	if v1 != "45.2" || v2 != "15.0" {
		t.Errorf("ASCII 模式下的值 %q %q", v1, v2)
	}
	if line, _ := trafficLine(rx, false); !strings.Contains(line, "-:0103100000417") || !strings.Contains(line, " CRLF") {
		t.Errorf("ASCII 报文应按字符显示：%q", line)
	}
	text := rowsText(describePacket(rx, ws.points))
	for _, want := range []string{"起始符", "Slave ID = 1", "40347", "温差设定 15.0 ℃", "LRC", "校验正确", "CR LF"} {
		if !strings.Contains(text, want) {
			t.Errorf("ASCII 报文解析缺少 %q：\n%s", want, text)
		}
	}
	bad := rx
	bad.Raw = append([]byte(nil), rx.Raw...)
	bad.Raw[len(bad.Raw)-3] ^= 1
	if text := rowsText(describePacket(bad, nil)); !strings.Contains(text, "校验错误：收到") {
		t.Errorf("LRC 错误解析：\n%s", text)
	}
	if got := frameText(modbus.ModeASCII, []byte{0x01, 0x03}); got != "01 03" {
		t.Errorf("ASCII 模式下收到二进制应显示十六进制：%q", got)
	}
	locked(func() { ws.disconnect() })
}

func TestDescribeNewFunctions(t *testing.T) {
	d := func(dir modbus.Direction, req, pdu []byte) string {
		return rowsText(describePDUOnly(dir, req, pdu, nil))
	}
	devID := []byte{0x2B, 0x0E, 0x01, 0x82, 0x00, 0x00, 0x02, 0x00, 0x03, 'A', 'B', 'C', 0x01, 0x02, 'P', '1'}
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"FC43 读设备标识响应", d(modbus.DirRX, []byte{0x2B, 0x0E, 0x01, 0x00}, devID), []string{"读设备标识", "常规，支持流式和单个读取", "厂商名称", "ABC", "产品代码", "P1"}},
		{"FC43 请求", d(modbus.DirTX, nil, []byte{0x2B, 0x0E, 0x02, 0x00}), []string{"常规", "厂商名称"}},
		{"FC08 计数响应", d(modbus.DirRX, nil, []byte{0x08, 0x00, 0x0E, 0x00, 0x07}), []string{"本站报文计数", "计数", "7"}},
		{"FC08 只听模式请求", d(modbus.DirTX, nil, []byte{0x08, 0x00, 0x04, 0x00, 0x00}), []string{"进入只听模式", "不再应答"}},
		{"FC22 请求", d(modbus.DirTX, nil, []byte{0x16, 0x01, 0x5F, 0xFF, 0xF0, 0x00, 0x05}), []string{"22 掩码写寄存器", "40352", "AND 掩码", "1111111111110000", "结果"}},
		{"FC23 请求", d(modbus.DirTX, nil, []byte{0x17, 0x01, 0x5E, 0x00, 0x02, 0x01, 0x5F, 0x00, 0x01, 0x02, 0x02, 0x8A}), []string{"读起始地址", "40351", "写起始地址", "先写后读", "650"}},
		{"FC23 响应", d(modbus.DirRX, []byte{0x17, 0x01, 0x5E, 0x00, 0x02}, []byte{0x17, 0x04, 0x13, 0x88, 0x02, 0x8A}), []string{"40351", "5000", "40352", "650"}},
		{"FC17 报告从站 ID", d(modbus.DirRX, nil, []byte{0x11, 0x04, 'S', 'I', 'M', 0xFF}), []string{"从站 ID", "SIM", "运行"}},
		{"FC11 通信事件计数", d(modbus.DirRX, nil, []byte{0x0B, 0xFF, 0xFF, 0x00, 0x09}), []string{"忙", "9"}},
		{"FC07 异常状态", d(modbus.DirRX, nil, []byte{0x07, 0x05}), []string{"00000101"}},
		{"异常 08", d(modbus.DirRX, nil, []byte{0x94, 0x08}), []string{"20 读文件记录 的异常响应", "Memory Parity Error"}},
	}
	for _, c := range cases {
		for _, w := range c.want {
			if !strings.Contains(c.text, w) {
				t.Errorf("%s 缺少 %q：\n%s", c.name, w, c.text)
			}
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

func TestDiagCounters(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeRTUOverTCP, 1, simulator.HeatStation())
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := modbus.NewClient(conn, modbus.Options{Mode: modbus.ModeRTUOverTCP})
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Quantity: 1})
	}
	counts, err := readDiagCounters(context.Background(), c, 1)
	if err != nil || len(counts) != 8 || counts[0].sub != 0x0B || counts[0].n != 4 || counts[3].sub != 0x0E || counts[3].n != 7 {
		t.Fatalf("诊断计数器 %+v %v", counts, err)
	}
}

// 连接期间的全部收发存进 SQLite，每次连接一个会话；历史报文窗口能选会话并载入记录。
func TestRecordingAndHistory(t *testing.T) {
	r, err := recorder.Open(filepath.Join(t.TempDir(), "packets.db"))
	if err != nil {
		t.Fatal(err)
	}
	SetRecorder(r, "packets.db", nil)
	t.Cleanup(func() {
		SetRecorder(nil, "", nil)
		r.Close()
	})
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() { ws.addWindow(defaultDef()) })
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "收到数据", func() bool { return hasData(ws.windows[0]) })
	locked(func() { ws.disconnect() })
	var ss []recorder.Session
	waitFor(t, 3*time.Second, "记录写入数据库", func() bool {
		ss, _ = r.Sessions(10)
		return len(ss) == 1 && ss[0].Packets >= 2 && !ss[0].End.IsZero()
	})
	if ss[0].Mode != modbus.ModeRTUOverTCP || !strings.Contains(ss[0].Target, "内置模拟器") || ss[0].Window != ws.no {
		t.Errorf("会话 %+v", ss[0])
	}
	ps, err := r.Packets(ss[0].ID, 10)
	if err != nil || ps[0].Dir != modbus.DirTX || ps[0].Status != modbus.StatusSent || len(ps[0].Raw) != 8 {
		t.Errorf("第一条应是请求：%+v %v", ps, err)
	}

	locked(func() { ws.openHistory() })
	hw := ws.tools[len(ws.tools)-1]
	var sel *widget.Select
	var labels []*widget.Label
	var walk func(o fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Select:
			sel = x
		case *widget.Label:
			labels = append(labels, x)
		case *fyne.Container:
			for _, c := range x.Objects {
				walk(c)
			}
		}
	}
	walk(hw.Content())
	if sel == nil || len(sel.Options) != 1 || !strings.Contains(sel.Options[0], "内置模拟器") {
		t.Fatalf("历史报文应列出这次连接：%v", sel)
	}
	locked(func() { sel.SetSelected(sel.Options[0]) })
	waitFor(t, 3*time.Second, "载入记录", func() bool {
		for _, l := range labels {
			if strings.HasPrefix(l.Text, "共 ") {
				return true
			}
		}
		return false
	})
}

// ASCII 模式下 LRC 错误要和 CRC 错误一样给出原因分析，不能只显示原始错误文字。
func TestLRCDiagnosis(t *testing.T) {
	srv := simulator.NewServer(modbus.ModeASCIIOverTCP, 1, simulator.HeatStation())
	srv.SetFaults(simulator.Faults{CRCRate: 1})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	var w *readWindow
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.target.SetText(ln.Addr().String())
		ws.proto.SetSelected(protoASCIITCP)
		w = ws.addWindow(defaultDef())
	})
	tap(ws.connBtn)
	waitFor(t, 5*time.Second, "LRC 错误的分析", func() bool {
		return w.errLbl.Text == "LRC 错误：响应校验失败" && strings.Contains(w.hintLbl.Text, "波特率")
	})
	tap(ws.connBtn)
}
