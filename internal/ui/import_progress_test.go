package ui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// findProgress 找出界面里的进度条。
func findProgress(o fyne.CanvasObject) *widget.ProgressBar {
	switch x := o.(type) {
	case *widget.ProgressBar:
		return x
	case *fyne.Container:
		for _, c := range x.Objects {
			if p := findProgress(c); p != nil {
				return p
			}
		}
	case fyne.Widget:
		for _, c := range test.WidgetRenderer(x).Objects() {
			if p := findProgress(c); p != nil {
				return p
			}
		}
	}
	return nil
}

// 大点表导入时显示进度、已用时间和预计剩余时间；解析到一半点“取消”，解析马上停下，原点表不变。
func TestLargePointImportShowsProgressAndCancels(t *testing.T) {
	rows := [][]string{{"属性标识", "属性名称", "读写模式", "单位", "计算公式", "数据类型", "模式"}}
	for i := range 200000 {
		rows = append(rows, []string{fmt.Sprintf("4x%05d:REAL", 1+i*2%60000), fmt.Sprintf("点%d", i), "1", "℃", "x*0.1", "2", "0"})
	}
	data := makeXLSX(t, rows)
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.setPoints(newPointTable([]point{{Area: modbus.AreaHoldingRegisters, Offset: 20, Type: modbus.TypeUint16, Name: "原点"}}))
		ws.startPointImport(&importLoadReader{ReadCloser: io.NopCloser(bytes.NewReader(data)), uri: storage.NewFileURI("大点表.xlsx")})
	})
	var bar *widget.ProgressBar
	var text string
	waitFor(t, 30*time.Second, "解析进度过了一成", func() bool {
		top := ws.win.Canvas().Overlays().Top()
		if top == nil {
			return false
		}
		bar, text = findProgress(top), overlayText(ws)
		return bar != nil && bar.Value > 0.15 && strings.Contains(text, "% · 已用")
	})
	locked(func() {
		if bar.Value >= 1 || !strings.Contains(text, "正在解析点表：大点表.xlsx") {
			t.Fatalf("应在解析中途：%v，%s", bar.Value, text)
		}
		test.Tap(findButtons(ws.win.Canvas().Overlays().Top(), "取消")[0])
		if ws.importTask != nil {
			t.Error("取消应立即释放导入状态")
		}
	})
	time.Sleep(500 * time.Millisecond) // 后台解析如果没停，结果会在这之后被丢弃，原点表不受影响也看不出来；这里只看界面
	locked(func() {
		if p, ok := ws.points.Get(modbus.AreaHoldingRegisters, 20); !ok || p.Name != "原点" || len(ws.points) != 1 {
			t.Errorf("取消后应保留原点表：%v", ws.points)
		}
		if ws.win.Canvas().Overlays().Top() != nil {
			t.Errorf("取消后不应再弹出导入结果：%s", overlayText(ws))
		}
	})
}

func TestProgressText(t *testing.T) {
	start := time.Now().Add(-4 * time.Second)
	if got := progressText(0.4, start); got != "40% · 已用 4 秒 · 预计还要 6 秒" {
		t.Errorf("进度说明：%q", got)
	}
	if got := progressText(0.01, start); strings.Contains(got, "预计") {
		t.Errorf("进度太少时估不准，不报剩余时间：%q", got)
	}
	if got := roughDuration(125 * time.Second); got != "2 分 5 秒" {
		t.Errorf("时长：%q", got)
	}
}
