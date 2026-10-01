package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
)

// workspaceFile 是保存到磁盘的工作区：连接参数、读取窗口和点表，一个 JSON 文件自带全部内容。
type workspaceFile struct {
	Mode      modbus.Mode `json:"mode"`
	Target    string      `json:"target"`
	UseSim    bool        `json:"useSim"`
	Port      string      `json:"port,omitempty"`
	Baud      string      `json:"baud,omitempty"`
	Format    string      `json:"format,omitempty"`
	TimeoutMS int64       `json:"timeoutMs"`
	Windows   []readDef   `json:"windows"`
	Points    []point     `json:"points,omitempty"`
	ReadOnly  bool        `json:"readOnly,omitempty"`
}

const recentKey = "recentWorkspaces"

func (ws *Workspace) encodeWorkspace() ([]byte, error) {
	f := workspaceFile{Mode: protoModes[ws.proto.Selected], Target: ws.target.Text, UseSim: ws.useSim.Checked,
		Port: ws.port.Selected, Baud: ws.baud.Text, Format: ws.frameFmt.Selected, TimeoutMS: ws.timeout.Milliseconds(),
		Points: ws.points.list(), ReadOnly: ws.readOnly}
	for _, w := range ws.windows {
		f.Windows = append(f.Windows, w.def)
	}
	return json.MarshalIndent(f, "", "  ")
}

// applyWorkspace 用文件内容替换当前工作区：先断开，再换连接参数、点表和读取窗口。打开后不自动连接。
func (ws *Workspace) applyWorkspace(data []byte) error {
	var f workspaceFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("不是有效的工作区文件：%w", err)
	}
	for i, d := range f.Windows {
		if err := d.validate(); err != nil {
			return fmt.Errorf("读取窗口 %d：%w", i+1, err)
		}
	}
	proto := protoName(f.Mode)
	if proto == "" {
		return fmt.Errorf("不认识的协议 %q", f.Mode)
	}
	ws.disconnect()
	for len(ws.windows) > 0 {
		ws.removeWindow(ws.windows[0])
	}
	ws.proto.SetSelected(proto)
	ws.target.SetText(f.Target)
	ws.useSim.SetChecked(f.UseSim)
	if f.Port != "" {
		ws.port.SetSelected(f.Port) // 串口没插时不在列表里，也照样显示，插上后能直接连
	}
	if f.Baud != "" {
		ws.baud.SetText(f.Baud)
	}
	if f.Format != "" {
		ws.frameFmt.SetSelected(f.Format)
	}
	if f.TimeoutMS > 0 {
		ws.setTimeout(time.Duration(f.TimeoutMS) * time.Millisecond)
	}
	ws.points = newPointTable(f.Points)
	ws.setReadOnly(f.ReadOnly)
	for _, d := range f.Windows {
		ws.addWindow(d)
	}
	return nil
}

func (ws *Workspace) setPath(path string) {
	ws.path = path
	ws.refreshTitle()
	if path == "" {
		return
	}
	prefs := ws.app.Preferences()
	recent := slices.DeleteFunc(prefs.StringList(recentKey), func(p string) bool { return p == path })
	recent = append([]string{path}, recent...)
	prefs.SetStringList(recentKey, recent[:min(len(recent), 5)])
}

// openWorkspaceFile 打开最近使用的工作区。
func (ws *Workspace) openWorkspaceFile(path string) {
	data, err := os.ReadFile(path)
	if err == nil {
		err = ws.applyWorkspace(data)
	}
	if err != nil {
		prefs := ws.app.Preferences()
		prefs.SetStringList(recentKey, slices.DeleteFunc(prefs.StringList(recentKey), func(p string) bool { return p == path }))
		ws.relayout()
		dialog.ShowError(fmt.Errorf("打开 %s 失败：%w", filepath.Base(path), err), ws.win)
		return
	}
	ws.setPath(path)
}

func (ws *Workspace) openWorkspace() {
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err == nil {
			err = ws.applyWorkspace(data)
		}
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.setPath(rc.URI().Path())
	}, ws.win)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".json"}))
	d.Show()
}

// saveWorkspace 已有文件时直接覆盖保存，否则弹出另存为。
func (ws *Workspace) saveWorkspace() {
	if ws.path == "" {
		ws.saveWorkspaceAs()
		return
	}
	data, err := ws.encodeWorkspace()
	if err == nil {
		err = os.WriteFile(ws.path, data, 0o644)
	}
	if err != nil {
		dialog.ShowError(err, ws.win)
	}
}

func (ws *Workspace) saveWorkspaceAs() {
	d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil || wc == nil {
			return
		}
		defer wc.Close()
		data, err := ws.encodeWorkspace()
		if err == nil {
			_, err = wc.Write(data)
		}
		if err != nil {
			dialog.ShowError(err, ws.win)
			return
		}
		ws.setPath(wc.URI().Path())
	}, ws.win)
	d.SetFileName("工作区.json")
	d.Show()
}

// setPoints 换点表后重建各读取窗口的列（名称、单位列随点表出现或消失）。
func (ws *Workspace) setPoints(pts pointTable) {
	ws.points = pts
	for _, w := range ws.windows {
		w.setDef(w.def)
	}
	ws.relayout()
	ws.refreshStatus()
}

// importPoints 导入点表：本程序的 CSV / xlsx 点表，或物联网平台导出的设备属性表（xlsx / CSV）。
func (ws *Workspace) importPoints() {
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		var imp pointImport
		if err == nil {
			imp, err = parsePointsFile(rc.URI().Name(), data)
		}
		if err != nil {
			dialog.ShowError(fmt.Errorf("导入点表失败：%w", err), ws.win)
			return
		}
		ws.showImportResult(ws.applyImport(imp))
	}, ws.win)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".csv", ".xlsx"}))
	d.Show()
}

// applyImport 换上导入的点表，返回给用户看的说明。已有的寄存器读取窗口改成按点表显示，省得逐个改显示格式；
// 还没有寄存器读取窗口时，按点表的地址新建。
func (ws *Workspace) applyImport(imp pointImport) string {
	ws.setPoints(newPointTable(imp.points))
	lines := []string{fmt.Sprintf("从%s导入了 %d 个点。", imp.format, len(imp.points))}
	converted := false
	for _, w := range ws.windows {
		if w.def.bits() {
			continue
		}
		converted = true
		if w.def.Kind != kindPoint {
			d := w.def
			d.Kind = kindPoint
			ws.applyDef(w, d)
		}
	}
	if converted {
		lines = append(lines, "读取窗口按点表显示名称、单位和工程值。")
	} else {
		var spans []string
		for _, d := range defsForPoints(imp.points) {
			ws.addWindow(d)
			spans = append(spans, refSpan(d.area(), d.Start, d.Qty))
		}
		lines = append(lines, "按点表新建了读取窗口："+strings.Join(spans, "、")+"。")
	}
	if imp.noOrder && slices.ContainsFunc(imp.points, func(p point) bool { return p.Type != typeString && p.regs() > 1 }) {
		lines = append(lines, "文件里没有字节序，32 / 64 位点先按 ABCD（Telegraf 的默认值）解码；数值不对时读取窗口会提示改用哪种，一键改好。")
	}
	if n := len(imp.skipped); n > 0 {
		lines = append(lines, fmt.Sprintf("\n跳过 %d 行：", n))
		lines = append(lines, imp.skipped[:min(n, 12)]...)
		if n > 12 {
			lines = append(lines, "……")
		}
	}
	if len(imp.notes) > 0 {
		lines = append(lines, "")
		lines = append(lines, imp.notes...)
	}
	return strings.Join(lines, "\n")
}

func (ws *Workspace) showImportResult(msg string) {
	l := widget.NewLabel(msg)
	l.Wrapping = fyne.TextWrapWord
	l.Resize(fyne.NewSize(480, 0)) // 自动换行的标签要先定宽度，MinSize 才是换行后的高度
	scroll := container.NewVScroll(l)
	scroll.SetMinSize(fyne.NewSize(480, min(l.MinSize().Height, 360)))
	dialog.NewCustom("导入点表", "好", scroll, ws.win).Show()
}

// defsForPoints 按点表的地址生成读取定义：同一数据区里相近的点合成一个窗口，一次最多读 120 个寄存器，
// 相隔 20 个寄存器以上另开窗口；最多 8 个窗口。
func defsForPoints(ps []point) []readDef {
	sorted := slices.Clone(ps)
	slices.SortFunc(sorted, func(a, b point) int {
		if a.Area != b.Area {
			return int(a.Area) - int(b.Area)
		}
		return int(a.Offset) - int(b.Offset)
	})
	var out []readDef
	for _, p := range sorted {
		end := int(p.Offset) + p.regs()
		if n := len(out); n > 0 {
			d := &out[n-1]
			if d.area() == p.Area && end-int(d.Start) <= 120 && int(p.Offset)-(int(d.Start)+d.Qty) <= 20 {
				d.Qty = max(d.Qty, end-int(d.Start))
				continue
			}
		}
		if len(out) == 8 {
			break
		}
		d := defaultDef()
		d.Function = modbus.FuncReadHoldingRegisters
		if p.Area == modbus.AreaInputRegisters {
			d.Function = modbus.FuncReadInputRegisters
		}
		d.Start, d.Qty, d.Kind = p.Offset, p.regs(), kindPoint
		out = append(out, d)
	}
	return out
}

// setPointOrder 把点表里按 from 解码的 32 / 64 位数值点全部改成 to（都用 32 位写法），用于一键改正字节序。
func (ws *Workspace) setPointOrder(from, to modbus.ByteOrder) {
	pts := pointTable{}
	for k, p := range ws.points {
		if p.Type != typeString && p.regs() > 1 && p.Order == from.For(p.Type) {
			p.Order = to.For(p.Type)
		}
		pts[k] = p
	}
	ws.setPoints(pts)
}

// recent 返回最近打开过、且文件还在的工作区，最多 5 个。
func (ws *Workspace) recent() []string {
	var out []string
	for _, p := range ws.app.Preferences().StringList(recentKey) {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
