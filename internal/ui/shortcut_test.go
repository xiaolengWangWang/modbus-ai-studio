package ui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// pressShortcut 按 Fyne 桌面驱动的规则触发快捷键：在主菜单里找 ShortcutName 相同的菜单项，执行它的动作。
// 返回菜单项的名称，没有对应的菜单项时为空。
func pressShortcut(ws *Workspace, k fyne.KeyName, shift bool) string {
	mod := fyne.KeyModifierShortcutDefault
	if shift {
		mod |= fyne.KeyModifierShift
	}
	name := (&desktop.CustomShortcut{KeyName: k, Modifier: mod}).ShortcutName()
	for _, m := range ws.win.MainMenu().Items {
		for _, it := range m.Items {
			if it.Shortcut != nil && it.Shortcut.ShortcutName() == name {
				it.Action()
				return it.Label
			}
		}
	}
	return ""
}

// 菜单快捷键不重复，常用功能都有；“读取”菜单里的定义、暂停作用于当前读取窗口（最近点过的那个，标题高亮）。
func TestShortcuts(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		seen := map[string]string{}
		for _, m := range ws.win.MainMenu().Items {
			for _, it := range m.Items {
				if it.Shortcut == nil {
					continue
				}
				// Windows、Linux 上 Ctrl 加这几个键被 Fyne 当成编辑快捷键，到不了菜单；⌘H、⌘M、⌘Q 是 macOS 系统的
				if s := it.Shortcut.(*desktop.CustomShortcut); s.Modifier == fyne.KeyModifierShortcutDefault {
					switch s.KeyName {
					case fyne.KeyA, fyne.KeyC, fyne.KeyV, fyne.KeyX, fyne.KeyZ, fyne.KeyY, fyne.KeyH, fyne.KeyM, fyne.KeyQ:
						t.Errorf("“%s”的快捷键 %s 会被系统或输入框占用", it.Label, s.KeyName)
					}
				}
				if prev, dup := seen[it.Shortcut.ShortcutName()]; dup {
					t.Errorf("“%s”和“%s”用了同一个快捷键", prev, it.Label)
				}
				seen[it.Shortcut.ShortcutName()] = it.Label
			}
		}
		for _, c := range []struct {
			key   fyne.KeyName
			shift bool
			label string
		}{
			{fyne.KeyN, false, "新建窗口"}, {fyne.KeyO, false, "打开工作区…"}, {fyne.KeyS, false, "保存工作区"},
			{fyne.KeyS, true, "工作区另存为…"}, {fyne.KeyW, false, "关闭窗口"}, {fyne.KeyK, false, "连接 / 断开"},
			{fyne.KeyD, false, "识别协议"}, {fyne.KeyT, false, "新建读取窗口"}, {fyne.KeyE, false, "读取定义…"},
			{fyne.KeyReturn, false, "写入选中的值…"}, {fyne.KeyP, false, "暂停 / 继续"}, {fyne.KeyI, false, "导入点表…"},
			{fyne.KeyP, true, "全部暂停"}, {fyne.KeyR, true, "全部继续"}, {fyne.KeyR, false, "自定义请求…"},
			{fyne.KeyH, true, "历史报文…"}, {fyne.KeyL, false, "清空通信报文"}, {fyne.KeyB, false, "功能码 / 数据类型 / 字节序调试…"},
		} {
			name := (&desktop.CustomShortcut{KeyName: c.key, Modifier: fyne.KeyModifierShortcutDefault}).ShortcutName()
			if c.shift {
				name = (&desktop.CustomShortcut{KeyName: c.key, Modifier: fyne.KeyModifierShortcutDefault | fyne.KeyModifierShift}).ShortcutName()
			}
			if seen[name] != c.label {
				t.Errorf("%s 应是“%s”，实际是“%s”", name, c.label, seen[name])
			}
		}

		// 没有读取窗口时，作用于当前窗口的快捷键什么也不做
		if pressShortcut(ws, fyne.KeyP, false) == "" || ws.current() != nil {
			t.Fatal("没有读取窗口时不应有当前窗口")
		}
		ws.loadDemo()
	})
	var wins []*readWindow
	locked(func() { wins = append(wins, ws.windows...) })
	if len(wins) != 3 {
		t.Fatalf("示例应有 3 个读取窗口，实际 %d", len(wins))
	}
	locked(func() {
		if ws.current() != wins[0] || ws.mdi.top() != wins[0].inner {
			t.Errorf("打开示例后当前窗口应是第一个，在最上面")
		}
		wins[2].tapCell(widget.TableCellID{Row: 0, Col: 0})
		if ws.current() != wins[2] || wins[2].sel != 0 {
			t.Errorf("点了窗口 3 的值后它应成为当前窗口")
		}
		wins[0].tapCell(widget.TableCellID{Row: 0, Col: 0})
		if ws.current() != wins[0] || ws.mdi.top() != wins[0].inner {
			t.Errorf("点了窗口 1 后它应成为当前窗口并提到最上面")
		}
		if wins[2].sel != -1 {
			t.Errorf("换到窗口 1 后窗口 3 的选中应取消，同一时间只有一个选中的值：sel=%d", wins[2].sel)
		}
		// 点窗口里不响应点击的地方（状态行、空白）也设为当前窗口
		test.Tap(wins[1].root)
		if ws.current() != wins[1] || ws.mdi.top() != wins[1].inner || wins[0].sel != -1 {
			t.Errorf("点窗口 2 的空白处应设为当前窗口、提到最上面并取消窗口 1 的选中")
		}
		wins[0].tapCell(widget.TableCellID{Row: 0, Col: 0})
		pressShortcut(ws, fyne.KeyP, false)
		if !wins[0].paused || wins[1].paused || wins[2].paused {
			t.Errorf("⌘P 应只暂停当前窗口：%v %v %v", wins[0].paused, wins[1].paused, wins[2].paused)
		}
		pressShortcut(ws, fyne.KeyP, true)
		if !wins[1].paused || !wins[2].paused {
			t.Error("⌘⇧P 应暂停全部")
		}
		pressShortcut(ws, fyne.KeyR, true)
		if wins[0].paused || wins[1].paused || wins[2].paused {
			t.Error("⌘⇧R 应继续全部")
		}
		// 当前窗口关掉后，下面一层的窗口（刚才点过的窗口 2）成为当前窗口
		ws.removeWindow(wins[0])
		if ws.current() != wins[1] || ws.mdi.top() != wins[1].inner {
			t.Error("当前窗口关掉后应取下面一层的窗口")
		}
		ws.removeWindow(wins[2])
		if ws.current() != wins[1] || len(ws.mdi.box.Objects) != 1 {
			t.Error("只剩窗口 2")
		}
		pressShortcut(ws, fyne.KeyE, false)
		if len(ws.win.Canvas().Overlays().List()) == 0 {
			t.Error("⌘E 应打开当前窗口的读取定义")
		}

		// 读取窗口里方向键从点过的格子接着移动并直接选中（Modbus Poll 的习惯），空格不改变选中
		w := ws.addWindow(defaultDef())
		w.tapCell(widget.TableCellID{Row: 4, Col: 1})
		w.table.TypedKey(&fyne.KeyEvent{Name: fyne.KeyDown})
		w.table.TypedKey(&fyne.KeyEvent{Name: fyne.KeySpace})
		if w.sel != 5 || ws.current() != w {
			t.Errorf("方向键应选中下一行：sel=%d", w.sel)
		}
	})
}
