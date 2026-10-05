package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// overlayWidgets 取出最上层对话框里的输入框、下拉框和按钮，按出现顺序。
func overlayWidgets(ws *Workspace) (entries []*widget.Entry, selects []*widget.Select, buttons map[string]*widget.Button) {
	buttons = map[string]*widget.Button{}
	top := ws.win.Canvas().Overlays().Top()
	if top == nil {
		return
	}
	var walk func(o fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch x := o.(type) {
		case *widget.Entry:
			entries = append(entries, x)
		case *widget.Select:
			selects = append(selects, x)
		case *widget.Button:
			buttons[x.Text] = x
		}
		if w, ok := o.(fyne.Widget); ok {
			if _, isEntry := o.(*widget.Entry); !isEntry {
				for _, c := range test.WidgetRenderer(w).Objects() {
					walk(c)
				}
			}
		}
		if c, ok := o.(*fyne.Container); ok {
			for _, x := range c.Objects {
				walk(x)
			}
		}
	}
	walk(top)
	return
}

func pressButton(t *testing.T, ws *Workspace, text string) {
	t.Helper()
	_, _, buttons := overlayWidgets(ws)
	b, ok := buttons[text]
	if !ok {
		t.Fatalf("对话框里没有“%s”按钮：%s", text, overlayText(ws))
	}
	test.Tap(b)
}

// 读取定义：改了确定才生效，不丢“原始值”开关；Slave ID 不对时报错、不改；取消不改。
func TestDefinitionDialog(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		d := defaultDef()
		d.Raw = true
		w := ws.addWindow(d)

		ws.showDefinition(w)
		entries, _, _ := overlayWidgets(ws) // 名称、Slave ID、起始地址、数量、扫描周期
		if len(entries) < 5 {
			t.Fatalf("读取定义应有 5 个输入框，找到 %d 个", len(entries))
		}
		entries[3].SetText("16")
		pressButton(t, ws, "确定")
		if w.def.Qty != 16 || !w.def.Raw {
			t.Errorf("确定后数量应为 16 且保留原始值开关：%+v", w.def)
		}

		clearOverlays(ws)
		ws.showDefinition(w)
		entries, _, _ = overlayWidgets(ws)
		entries[1].SetText("0")
		pressButton(t, ws, "确定")
		if w.def.Slave != 1 || !strings.Contains(overlayText(ws), "Slave ID") {
			t.Errorf("Slave ID 为 0 时应报错且不生效：Slave %d\n%s", w.def.Slave, overlayText(ws))
		}

		clearOverlays(ws)
		ws.showDefinition(w)
		entries, _, _ = overlayWidgets(ws)
		entries[3].SetText("5")
		pressButton(t, ws, "取消")
		if w.def.Qty != 16 {
			t.Errorf("取消不应改定义：数量 %d", w.def.Qty)
		}
	})
}

// 调整点表字节序：选范围和字节序后应用，告诉用户改了几个点。
func TestPointOrderDialog(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.setPoints(newPointTable([]point{
			{Area: modbus.AreaHoldingRegisters, Offset: 0, Name: "温度", Type: modbus.TypeFloat32, Order: modbus.OrderABCD, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 2, Name: "累计", Type: modbus.TypeUint64, Order: modbus.OrderABCDEFGH, Scale: 1},
			{Area: modbus.AreaHoldingRegisters, Offset: 10, Name: "状态", Type: modbus.TypeUint16, Order: modbus.OrderAB, Scale: 1},
		}))
		d := defaultDef()
		d.Kind, d.Qty = kindPoint, 4
		w := ws.addWindow(d)
		ws.showPointOrderDialog(w)
		_, selects, _ := overlayWidgets(ws) // 范围、字节序
		if len(selects) != 2 {
			t.Fatalf("应有范围和字节序两个下拉框，找到 %d 个", len(selects))
		}
		selects[0].SetSelected("全部点")
		selects[1].SetSelected("DCBA")
		pressButton(t, ws, "应用")
		for off, want := range map[uint16]modbus.ByteOrder{0: modbus.OrderDCBA, 2: modbus.OrderHGFEDCBA, 10: modbus.OrderAB} {
			if p, _ := ws.points.get(modbus.AreaHoldingRegisters, off); p.Order != want {
				t.Errorf("40%03d 字节序 %s，期望 %s（16 位点不受影响，64 位换成八字母写法）", off+1, p.Order, want)
			}
		}
		if !strings.Contains(overlayText(ws), "已调整 2 个") {
			t.Errorf("应说明调整了 2 个点：%s", overlayText(ws))
		}
	})
}

// 写入：点表点按工程值写，控制验证 PASS；16 位 BA 窗口按字节交换写到设备上。
func TestWriteDialogs(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() { ws.loadDemo() })
	w2 := ws.windows[1] // 40347 温差设定，FLOAT32 CDAB，可写 5–25
	waitFor(t, 5*time.Second, "读到数据", func() bool { return hasData(w2) })
	locked(func() {
		w2.tapCell(widget.TableCellID{Row: 0, Col: 2})
		ws.showWrite(w2)
		entries, _, _ := overlayWidgets(ws)
		if len(entries) == 0 {
			t.Fatal("写入对话框没有输入框")
		}
		entries[0].SetText("16.5")
		pressButton(t, ws, "确认写入")
	})
	waitFor(t, 5*time.Second, "控制验证 PASS", func() bool { return strings.Contains(overlayText(ws), "控制验证 PASS") })
	waitFor(t, 5*time.Second, "读到新值", func() bool { v, _ := w2.valueText(0); return v == "16.5" })

	var w *readWindow
	locked(func() {
		clearOverlays(ws)
		d := defaultDef()
		d.Start, d.Qty, d.Kind, d.Order = 800, 1, kindHex, modbus.OrderBA
		w = ws.addWindow(d)
	})
	waitFor(t, 5*time.Second, "读到 40801", func() bool { return hasData(w) })
	locked(func() {
		w.tapCell(widget.TableCellID{Row: 0, Col: 1})
		ws.showWrite(w)
		entries, _, _ := overlayWidgets(ws)
		entries[0].SetText("0x1234")
		pressButton(t, ws, "确认写入")
	})
	waitFor(t, 5*time.Second, "控制验证 PASS", func() bool { return strings.Contains(overlayText(ws), "控制验证 PASS") })
	var c *modbus.Client
	locked(func() { c = ws.session.client })
	resp, err := c.Do(context.Background(), modbus.Request{Slave: 1, Function: modbus.FuncReadHoldingRegisters, Address: 800, Quantity: 1})
	if err != nil || len(resp.Registers) != 1 || resp.Registers[0] != 0x3412 {
		t.Errorf("BA 窗口写 0x1234，设备上应是 0x3412：%v %v", resp.Registers, err)
	}
	locked(func() { clearOverlays(ws) })
	tap(ws.connBtn)
}
