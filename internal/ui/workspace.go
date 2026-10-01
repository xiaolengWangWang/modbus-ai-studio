package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"

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

func (ws *Workspace) importPoints() {
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		var ps []point
		if err == nil {
			ps, err = parsePointsCSV(data)
		}
		if err != nil {
			dialog.ShowError(fmt.Errorf("导入点表失败：%w", err), ws.win)
			return
		}
		ws.setPoints(newPointTable(ps))
		// 已有的寄存器读取窗口直接改成按点表显示，省得逐个改显示格式
		for _, w := range ws.windows {
			if !w.def.bits() && w.def.Kind != kindPoint {
				d := w.def
				d.Kind = kindPoint
				ws.applyDef(w, d)
			}
		}
		dialog.ShowInformation("导入点表", fmt.Sprintf("已导入 %d 个点，读取窗口按点表显示名称、单位和工程值。", len(ps)), ws.win)
	}, ws.win)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".csv"}))
	d.Show()
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
