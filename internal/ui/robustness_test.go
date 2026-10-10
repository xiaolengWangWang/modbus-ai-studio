package ui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

func TestRegisterProbeContinuesPastRepeatedDisconnects(t *testing.T) {
	for _, mode := range []probeMode{probeAuto, probeSingle, probePoints} {
		t.Run(probeModeNames[mode], func(t *testing.T) {
			ws, _ := tcpProbeWorkspace(t)
			waitFor(t, 5*time.Second, "连接", func() bool { return ws.session != nil })
			locked(func() {
				pts := newPointTable([]point{
					{Area: modbus.AreaHoldingRegisters, Offset: 0, Type: modbus.TypeUint16, Name: "温度"},
					{Area: modbus.AreaHoldingRegisters, Offset: 1, Type: modbus.TypeUint16, Name: "压力"},
					{Area: modbus.AreaHoldingRegisters, Offset: 2, Type: modbus.TypeUint16, Name: "状态"},
				})
				d, segs, _ := pointProbeDef(pts, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
				if mode != probePoints {
					segs = nil
				}
				ws.runRegisterProbeMode(nil, d, mode, segs)
			})
			waitFor(t, 10*time.Second, "检测完成", func() bool { return !ws.probeRunning })
			locked(func() {
				text := overlayText(ws)
				wants := []string{"可读（2）", "无法判断（1）", "未检测（0）"}
				if mode == probePoints {
					wants = []string{"3 个点：可读 2", "读取出错 1", "未检测 0", "40002 压力（UINT16）：读取出错"}
				}
				for _, want := range wants {
					if !strings.Contains(text, want) {
						t.Errorf("地址 1 反复断线后，应重连并继续检测地址 2；缺少 %s：%s", want, text)
					}
				}
			})
		})
	}
}

func tcpProbeWorkspace(t *testing.T) (*Workspace, *simulator.Server) {
	t.Helper()
	srv := simulator.NewServer(modbus.ModeTCP, 1, simulator.NewStore(3))
	srv.SetFaults(simulator.Faults{DisconnectOn: []simulator.AddrRange{{Start: 1, Count: 1}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.useSim.SetChecked(false)
		ws.proto.SetSelected(protoTCP)
		ws.target.SetText(ln.Addr().String())
		ws.connect()
	})
	return ws, srv
}

func TestRegisterProbeStartsWhileReconnectingAndCanStopWaiting(t *testing.T) {
	ws, srv := tcpProbeWorkspace(t)
	waitFor(t, 5*time.Second, "连接", func() bool { return ws.session != nil })
	var client *modbus.Client
	locked(func() { client = ws.session.client })
	_, _ = client.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 1, Quantity: 1})
	waitFor(t, 2*time.Second, "设备断开", func() bool { return lost(ws) })
	srv.SetFaults(simulator.Faults{DisconnectOnAccept: true})
	locked(func() {
		ws.runRegisterProbeMode(nil, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Qty: 3}, probeSingle, nil)
	})
	waitFor(t, 3*time.Second, "等待自动重连", func() bool { return strings.Contains(overlayText(ws), "等待自动重连") })
	locked(func() {
		buttons := findButtons(ws.win.Canvas().Overlays().Top(), "停止")
		if len(buttons) != 1 {
			t.Fatal("等待重连时必须能停止")
		}
		test.Tap(buttons[0])
	})
	waitFor(t, 2*time.Second, "停止检测", func() bool { return !ws.probeRunning })
	locked(func() {
		text := overlayText(ws)
		if !strings.Contains(text, "保留已完成") || !strings.Contains(text, "未检测（3）") {
			t.Errorf("等待重连时停止，应保留未检测状态：%s", text)
		}
	})
}

func TestImportSparsePointsBoundsWindowsAndKeepsAllPoints(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	var pts []point
	for i := range 1000 {
		pts = append(pts, point{Area: modbus.AreaHoldingRegisters, Offset: uint16(i * 60), Type: modbus.TypeUint16, Name: fmt.Sprintf("点%d", i)})
	}
	locked(func() {
		msg := ws.applyImport(pointImport{format: "点表", points: pts})
		if len(ws.windows) > 16 {
			t.Errorf("大点表不应自动创建无上限的窗口：创建了 %d 个", len(ws.windows))
		}
		if len(ws.points) != 1000 {
			t.Fatalf("限制窗口不能丢失点位：%d", len(ws.points))
		}
		_, segs, ok := pointProbeDef(ws.points, readDef{Slave: 1, Function: modbus.FuncReadHoldingRegisters})
		if !ok || len(segs) != 1000 || !strings.Contains(msg, "未自动建窗") {
			t.Errorf("完整点表必须保留供检测，并说明未自动创建的窗口：%d, %s", len(segs), msg)
		}
	})
}

// 只替换文件读取边界；解析、导入、取消和窗口渲染使用真实流程。
type pointImportReader struct {
	*bytes.Reader
	started, release, closed chan struct{}
	startOnce, closeOnce     sync.Once
}

func (r *pointImportReader) Read(p []byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	select {
	case <-r.closed:
		return 0, io.ErrClosedPipe
	case <-r.release:
		return r.Reader.Read(p)
	}
}
func (r *pointImportReader) Close() error  { r.closeOnce.Do(func() { close(r.closed) }); return nil }
func (r *pointImportReader) URI() fyne.URI { return storage.NewFileURI("points.csv") }

func TestPointImportDoesNotBlockUIAndCancellationKeepsExistingPoints(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	r := &pointImportReader{Reader: bytes.NewReader([]byte("地址,名称,类型\n40001,新点,UINT16\n")), started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { r.Close() })
	locked(func() {
		ws.setPoints(newPointTable([]point{{Area: modbus.AreaHoldingRegisters, Offset: 20, Type: modbus.TypeUint16, Name: "原点"}}))
		ws.startPointImport(r)
	})
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("后台读取未开始")
	}
	locked(func() {
		// 文件读取尚未返回，界面仍可响应取消。
		buttons := findButtons(ws.win.Canvas().Overlays().Top(), "取消")
		if len(buttons) != 1 {
			t.Fatal("导入必须提供取消")
		}
		test.Tap(buttons[0])
		if ws.importTask != nil {
			t.Error("取消应立即释放导入状态")
		}
	})
	select {
	case <-r.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("取消未关闭正在读取的文件")
	}
	locked(func() {
		p, ok := ws.points.get(modbus.AreaHoldingRegisters, 20)
		if !ok || p.Name != "原点" || len(ws.points) != 1 {
			t.Fatal("取消后必须保留原点表")
		}
	})
	// 取消后可以再次导入；旧任务不能覆盖新任务的结果。
	next := &pointImportReader{Reader: bytes.NewReader([]byte("地址,名称,类型\n40031,后续点,UINT16\n")), started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	close(next.release)
	locked(func() { ws.startPointImport(next) })
	waitFor(t, 5*time.Second, "重新导入完成", func() bool { return ws.importTask == nil })
	locked(func() {
		p, ok := ws.points.get(modbus.AreaHoldingRegisters, 30)
		if !ok || p.Name != "后续点" || len(ws.points) != 1 {
			t.Fatalf("取消的旧任务不得覆盖新导入：%v", ws.points)
		}
	})
}

func TestParseSheetSkipsBlankFormattingCells(t *testing.T) {
	b := []byte(`<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>地址</t></is></c><c r="B1" t="inlineStr"><is><t>名称</t></is></c><c r="C1" t="inlineStr"><is><t>类型</t></is></c><c r="XFD1" s="1"/></row><row r="2"><c r="A2"><v>40001</v></c><c r="B2" t="inlineStr"><is><t>温度</t></is></c><c r="C2" t="inlineStr"><is><t>UINT16</t></is></c><c r="XFD2" s="1"/></row><row r="1048576"><c r="XFD1048576" s="1"/></row></sheetData></worksheet>`)
	rows, err := parseSheet(b, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("只有格式的空行不应进入点表：%d, %v", len(rows), err)
	}
	for _, row := range rows {
		if len(row) != 3 {
			t.Errorf("末列空白格式不应使每行扩张到 16384 列：%d", len(row))
		}
	}
	imp, err := parsePointRowsAny(rows)
	if err != nil || len(imp.points) != 1 || imp.points[0].Name != "温度" {
		t.Fatalf("有效点位应仍可导入：%+v, %v", imp, err)
	}
}

func TestParseSheetKeepsInternalBlankColumnsWithoutReferences(t *testing.T) {
	rows, err := parseSheet([]byte(`<worksheet><sheetData><row><c><v>40001</v></c><c/><c t="inlineStr"><is><t>UINT16</t></is></c></row></sheetData></worksheet>`), nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 3 || rows[0][1] != "" || rows[0][2] != "UINT16" {
		t.Fatalf("删除尾部空白不能改变内部空列的位置：%v, %v", rows, err)
	}
}

func TestParseSheetRejectsInvalidReferences(t *testing.T) {
	for _, ref := range []string{"123", "XFE1", "ZZZZZZZZZZZZ1"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := parseSheet([]byte(fmt.Sprintf(`<worksheet><sheetData><row><c r="%s"><v>1</v></c></row></sheetData></worksheet>`, ref)), nil); err == nil {
				t.Fatal("无效单元格引用应返回错误，避免越界或大内存分配")
			}
		})
	}
}
