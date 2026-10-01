package ui

import (
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

func TestReadOnlyPDU(t *testing.T) {
	allow := [][]byte{{0x03, 0, 0, 0, 1}, {0x04, 0, 0, 0, 1}, {0x01, 0, 0, 0, 8}, {0x02, 0, 0, 0, 8}, {0x07}, {0x0B}, {0x11},
		{0x2B, 0x0E, 0x01, 0x00}, {0x08, 0, 0x00, 0x12, 0x34}, {0x08, 0, 0x0C, 0, 0}, {0x08, 0, 0x12, 0, 0}}
	deny := [][]byte{{0x05, 0, 0, 0xFF, 0}, {0x06, 0, 0, 0, 1}, {0x0F, 0, 0, 0, 1, 1, 1}, {0x10, 0, 0, 0, 1, 2, 0, 1},
		{0x16, 0, 0, 0, 0, 0, 0}, {0x17, 0, 0, 0, 1, 0, 0, 0, 1, 2, 0, 0}, {0x15, 0}, {0x08, 0, 0x01, 0, 0}, {0x08, 0, 0x04, 0, 0},
		{0x08, 0, 0x0A, 0, 0}, {0x2B, 0x0D}, {0x41, 0}, {}}
	for _, p := range allow {
		if !readOnlyPDU(p) {
			t.Errorf("只读模式应放行 % X", p)
		}
	}
	for _, p := range deny {
		if readOnlyPDU(p) {
			t.Errorf("只读模式应拦下 % X", p)
		}
	}
}

// 只读模式：写入按钮不可用、自定义请求拦下写类请求（报文里也看不到它），读请求照常；设置随工作区保存、新窗口继承。
func TestReadOnlyMode(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, true)
	w2 := ws.windows[1]
	waitFor(t, 5*time.Second, "示例数据", func() bool { return hasData(w2) })
	locked(func() {
		w2.table.Select(widget.TableCellID{Row: 0, Col: 2}) // 40347 温差设定，点表里可写
		if !w2.canWrite() {
			t.Fatal("关闭只读时温差设定应可写")
		}
		ws.setReadOnly(true)
		if w2.canWrite() || !w2.writeBtn.Disabled() || !ws.roItem.Checked || !strings.Contains(ws.status.Text, "只读模式") {
			t.Errorf("打开只读后：canWrite=%v 按钮禁用=%v 菜单勾选=%v 状态栏=%q", w2.canWrite(), w2.writeBtn.Disabled(), ws.roItem.Checked, ws.status.Text)
		}
		ws.openRequestTool()
	})
	tool := ws.tools[len(ws.tools)-1]
	var pdu *widget.Entry
	var send *widget.Button
	var walk func(o fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Entry:
			if x.PlaceHolder != "" && strings.HasPrefix(x.PlaceHolder, "PDU") {
				pdu = x
			}
		case *widget.Button:
			if x.Text == "发送" {
				send = x
			}
		case *fyne.Container:
			for _, c := range x.Objects {
				walk(c)
			}
		}
	}
	walk(tool.Content())
	countFC := func(fc modbus.FunctionCode) int {
		n := 0
		ws.traffic.flush()
		for _, p := range ws.traffic.all {
			if p.Dir == modbus.DirTX && p.Function == fc {
				n++
			}
		}
		return n
	}
	locked(func() { pdu.SetText("06 01 5F 02 8A") })
	tap(send)
	time.Sleep(300 * time.Millisecond)
	locked(func() {
		if n := countFC(modbus.FuncWriteSingleRegister); n != 0 {
			t.Errorf("只读模式下不应发出 FC06，报文里有 %d 条", n)
		}
		pdu.SetText("2B 0E 01 00") // 读设备标识照常
	})
	tap(send)
	waitFor(t, 3*time.Second, "读请求照常发送", func() bool { return countFC(modbus.FuncEncapsulatedInterface) == 1 })

	// 随工作区保存；新窗口继承
	var data []byte
	locked(func() { data, _ = ws.encodeWorkspace() })
	dst := openWS(t, a, false)
	var n *Workspace
	locked(func() {
		if err := dst.applyWorkspace(data); err != nil || !dst.readOnly {
			t.Errorf("工作区应恢复只读：%v %v", dst.readOnly, err)
		}
		n = ws.openNew()
	})
	t.Cleanup(func() { closeWS(n) })
	if !n.readOnly {
		t.Error("新窗口应继承只读")
	}
	locked(func() {
		ws.setReadOnly(false)
		if !w2.canWrite() {
			t.Error("关闭只读后应恢复可写")
		}
	})
}
