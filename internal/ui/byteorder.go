package ui

import (
	"fmt"
	"reflect"
	"slices"

	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/modbus"
)

// Raw values and local formats have their own display order; changing the
// unrelated point table cannot update them. Reinterpret retained data locally.
func (w *readWindow) showByteOrderDialog() {
	ws := w.ws
	selection := w.sel
	definition := w.def
	definition.Formats = slices.Clone(definition.Formats)
	const window, selected = "整个读取窗口", "选中的值"
	scopes := []string{window}
	current := window
	if w.sel >= 0 {
		scopes = append(scopes, selected)
		current = selected
	}
	scope := widget.NewSelect(scopes, nil)
	order := widget.NewSelect(nil, nil)
	update := func(string) {
		kind, cur := w.def.Kind, w.def.Order
		if scope.Selected == selected {
			f := w.valueFormat(selection)
			kind, cur = f.kind, f.order
		}
		order.Options = nil
		if scope.Selected == window && kind == kindPoint {
			opts, value, _ := w.orderChoice()
			order.Options = opts
			order.SetSelected(string(value))
		} else {
			for _, o := range kind.dataType().Orders() {
				order.Options = append(order.Options, string(o))
			}
			order.SetSelected(string(cur.For(kind.dataType())))
		}
		order.Refresh()
	}
	scope.OnChanged = update
	scope.SetSelected(current)
	dialog.NewForm("调整显示字节序", "应用", "取消", []*widget.FormItem{
		widget.NewFormItem("作用范围", scope), widget.NewFormItem("字节序", order),
	}, func(ok bool) {
		if !ok {
			return
		}
		if !slices.Contains(ws.windows, w) || !reflect.DeepEqual(w.def, definition) {
			dialog.ShowInformation("读取定义已改变", "请重新打开字节序调整。", ws.win)
			return
		}
		if scope.Selected == selected {
			f := w.valueFormat(selection)
			if err := w.setRegisterFormat(f.start, f.width, f.kind, modbus.ByteOrder(order.Selected)); err != nil {
				dialog.ShowError(err, ws.win)
				return
			}
		} else {
			ws.setWindowOrder(w, modbus.ByteOrder(order.Selected))
		}
		ws.status.SetText(fmt.Sprintf("窗口 %d：%s已按 %s 重新显示；原始报文不变", w.no, scope.Selected, order.Selected))
	}, ws.win).Show()
}
