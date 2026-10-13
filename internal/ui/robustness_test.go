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
		msg := ws.applyImport(pointImport{Format: "点表", Points: pts})
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
		p, ok := ws.points.Get(modbus.AreaHoldingRegisters, 20)
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
		p, ok := ws.points.Get(modbus.AreaHoldingRegisters, 30)
		if !ok || p.Name != "后续点" || len(ws.points) != 1 {
			t.Fatalf("取消的旧任务不得覆盖新导入：%v", ws.points)
		}
	})
}

