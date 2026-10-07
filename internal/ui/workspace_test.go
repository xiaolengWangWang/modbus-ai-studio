package ui

import (
	"reflect"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
)

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
		if len(dst.windows) != 3 || !reflect.DeepEqual(dst.windows[1].def, src.windows[1].def) || len(dst.points) != len(demoPoints()) {
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
