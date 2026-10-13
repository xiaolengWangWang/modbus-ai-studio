package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
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
		Points: ws.points.List(), ReadOnly: ws.readOnly}
	for _, w := range ws.windows {
		f.Windows = append(f.Windows, w.def)
	}
	return json.MarshalIndent(f, "", "  ")
}

// applyWorkspace 用文件内容替换当前工作区：先断开，再换连接参数、点表和读取窗口。打开后不自动连接。
func (ws *Workspace) applyWorkspace(data []byte) error {
	f, err := parseWorkspace(data)
	if err != nil {
		return err
	}
	ws.applyWorkspaceFile(f)
	return nil
}

// parseWorkspace 解析并检查工作区文件，不碰界面，可以在后台执行。
func parseWorkspace(data []byte) (workspaceFile, error) {
	var f workspaceFile
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("不是有效的工作区文件：%w", err)
	}
	for i, d := range f.Windows {
		if err := d.validate(); err != nil {
			return f, fmt.Errorf("读取窗口 %d：%w", i+1, err)
		}
	}
	if protoName(f.Mode) == "" {
		return f, fmt.Errorf("不认识的协议 %q", f.Mode)
	}
	return f, nil
}

func (ws *Workspace) applyWorkspaceFile(f workspaceFile) {
	proto := protoName(f.Mode)
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
	if len(ws.windows) > 1 {
		ws.mdi.setMaxed(true) // 和导入点表一样：最大化显示第一个窗口，上方标签切换
	}
}

// maxWorkspaceBytes 是工作区文件的上限，和点表文件一样；更大的多半选错了文件。
const maxWorkspaceBytes = 64 << 20

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

// loadWorkspace 在后台读取并解析工作区文件，界面线程只做最后的替换；读不完可以取消，原工作区不变。
// failed 在打开失败时于界面线程调用，可为空。
func (ws *Workspace) loadWorkspace(path string, open func() (io.ReadCloser, error), failed func()) {
	if ws.workspaceBusy {
		return
	}
	ws.workspaceBusy = true
	ws.fileTask(ws.win, "打开工作区", "正在读取工作区："+filepath.Base(path)+"（可取消）", func(ctx context.Context) (func(), error) {
		var f workspaceFile
		rc, err := open()
		if err == nil {
			var data []byte
			if data, err = readLimited(ctx, rc, maxWorkspaceBytes, "工作区文件"); err == nil {
				f, err = parseWorkspace(data)
			}
		}
		return func() {
			if err != nil {
				if failed != nil {
					failed()
				}
				dialog.ShowError(fmt.Errorf("打开 %s 失败：%w", filepath.Base(path), err), ws.win)
				return
			}
			ws.applyWorkspaceFile(f)
			ws.setPath(path)
		}, nil
	}, func() { ws.workspaceBusy = false })
}

// openWorkspaceFile 打开最近使用的工作区。
func (ws *Workspace) openWorkspaceFile(path string) {
	ws.loadWorkspace(path, func() (io.ReadCloser, error) { return os.Open(path) }, func() {
		prefs := ws.app.Preferences()
		prefs.SetStringList(recentKey, slices.DeleteFunc(prefs.StringList(recentKey), func(p string) bool { return p == path }))
		ws.relayout()
	})
}

func (ws *Workspace) openWorkspace() {
	if ws.workspaceBusy {
		return
	}
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		ws.loadWorkspace(rc.URI().Path(), func() (io.ReadCloser, error) { return rc, nil }, nil)
	}, ws.win)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".json"}))
	d.Show()
}

// saveWorkspace 已有文件时直接覆盖保存，否则弹出另存为。内容在界面线程取好，写盘在后台进行，
// 先写临时文件再替换，写到一半出错或取消时原文件不变。
func (ws *Workspace) saveWorkspace() {
	if ws.workspaceBusy {
		return
	}
	if ws.path == "" {
		ws.saveWorkspaceAs()
		return
	}
	data, err := ws.encodeWorkspace()
	if err != nil {
		dialog.ShowError(err, ws.win)
		return
	}
	path := ws.path
	ws.workspaceBusy = true
	ws.fileTask(ws.win, "保存工作区", "正在保存工作区："+filepath.Base(path), func(ctx context.Context) (func(), error) {
		return nil, writeFileAtomic(ctx, path, data)
	}, func() { ws.workspaceBusy = false })
}

func (ws *Workspace) saveWorkspaceAs() {
	if ws.workspaceBusy {
		return
	}
	d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil || wc == nil {
			return
		}
		data, err := ws.encodeWorkspace()
		if err != nil {
			wc.Close()
			dialog.ShowError(err, ws.win)
			return
		}
		path := wc.URI().Path()
		ws.workspaceBusy = true
		ws.fileTask(ws.win, "保存工作区", "正在保存工作区："+filepath.Base(path), func(ctx context.Context) (func(), error) {
			if err := writeAll(ctx, wc, data); err != nil {
				return nil, fmt.Errorf("保存 %s 失败：%w", filepath.Base(path), err)
			}
			return func() { ws.setPath(path) }, nil
		}, func() { ws.workspaceBusy = false })
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
	if ws.importTask != nil {
		return
	}
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		ws.startPointImport(rc)
	}, ws.win)
	d.SetFilter(storage.NewExtensionFileFilter([]string{".csv", ".xlsx"}))
	d.Show()
}

const maxPointFileBytes = 64 << 20
const maxImportWindows = 16

type pointImportTask struct{ cancel context.CancelFunc }

// startPointImport 读文件和解析都在后台执行，取消后丢弃结果，保留原点表。
func (ws *Workspace) startPointImport(rc fyne.URIReadCloser) {
	if ws.closed || ws.importTask != nil {
		go rc.Close()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := &pointImportTask{cancel: cancel}
	ws.importTask = task
	name := rc.URI().Name()
	label := widget.NewLabel("正在读取点表：" + name)
	label.Wrapping = fyne.TextWrapWord
	bar := widget.NewProgressBar()
	dlg := dialog.NewCustom("导入点表", "取消", container.NewVBox(label, bar), ws.win)
	dlg.Resize(fyne.NewSize(420, 0))
	dlg.SetOnClosed(func() {
		cancel()
		if ws.importTask == task {
			ws.importTask = nil
		}
	})
	dlg.Show()
	closeReader := sync.OnceFunc(func() { _ = rc.Close() })
	var size int64 // 本机文件先看大小，读的时候也能报进度
	if fi, err := os.Stat(rc.URI().Path()); err == nil {
		size = fi.Size()
	}
	go func() {
		stopClose := context.AfterFunc(ctx, closeReader)
		defer stopClose()
		defer closeReader()
		// 进度在后台报告，最多每 0.1 秒刷新一次界面；读文件占进度条的前 10%，解析占其余
		started, last := time.Now(), time.Time{}
		show := func(what string, frac float64) {
			if frac < 1 && time.Since(last) < 100*time.Millisecond {
				return
			}
			last = time.Now()
			text := what + name + "（可取消）\n" + progressText(frac, started)
			uiDo(func() {
				if ws.importTask == task && !ws.closed && ctx.Err() == nil {
					bar.SetValue(frac)
					label.SetText(text)
				}
			})
		}
		var r io.Reader = io.LimitReader(rc, maxPointFileBytes+1)
		if size > 0 {
			r = &progressReader{r: r, report: func(n int64) { show("正在读取点表：", 0.1*float64(min(n, size))/float64(size)) }}
		}
		var data []byte
		var err error
		if size > maxPointFileBytes { // 先看大小，太大的不用读完才报错
			err = fmt.Errorf("点表文件超过 64 MB，请拆分后导入")
		} else {
			data, err = io.ReadAll(r)
		}
		var imp pointImport
		if err == nil && len(data) > maxPointFileBytes {
			err = fmt.Errorf("点表文件超过 64 MB，请拆分后导入")
		}
		if err == nil && ctx.Err() == nil {
			imp, err = parsePoints(ctx, name, data, func(f float64) { show("正在解析点表：", 0.1+0.9*f) })
		}
		uiDo(func() {
			defer cancel()
			if ws.closed || ws.importTask != task {
				return
			}
			ws.importTask = nil
			cancelled := ctx.Err() != nil
			dlg.Hide()
			if cancelled {
				return
			}
			if err != nil {
				dialog.ShowError(fmt.Errorf("导入点表失败：%w", err), ws.win)
				return
			}
			ws.showImportResult(ws.applyImport(imp))
		})
	}()
}

// applyImport 换上导入的点表，已有读取窗口改成按点表显示；没有被完整覆盖的点自动补建窗口。
func (ws *Workspace) applyImport(imp pointImport) string {
	ws.setPoints(newPointTable(imp.Points))
	lines := []string{fmt.Sprintf("从%s导入了 %d 个点。", imp.Format, len(imp.Points))}
	converted := false
	for _, w := range ws.windows {
		if w.def.Kind != kindPoint {
			converted = true
			d := w.def
			d.Kind = kindPoint
			ws.applyDef(w, d)
		}
	}
	if converted {
		lines = append(lines, "读取窗口按点表显示名称、单位和工程值。")
	}
	var missing []point
	for _, p := range imp.Points {
		covered := slices.ContainsFunc(ws.windows, func(w *readWindow) bool {
			d := w.def
			return d.area() == p.Area && int(d.Start) <= int(p.Offset) && int(p.Offset)+p.Regs() <= int(d.Start)+d.Qty
		})
		if !covered {
			missing = append(missing, p)
		}
	}
	var spans []string
	defs := defsForPoints(missing)
	count := min(len(defs), max(0, maxImportWindows-len(ws.windows)))
	for _, d := range defs[:count] {
		if len(ws.windows) > 0 {
			d.Slave = ws.windows[0].def.Slave // 补建的窗口沿用已有窗口的站号，点表一般对应同一台设备
		}
		ws.addWindow(d)
		spans = append(spans, refSpan(d.area(), d.Start, d.Qty))
	}
	if remaining := len(defs) - count; remaining > 0 {
		lines = append(lines, fmt.Sprintf("还有 %d 个读取范围未自动建窗，完整点表已保留。为保持界面响应，自动建窗最多补到 %d 个；其余范围可在读取窗口的“定义”中调整，或用“检测寄存器 → 按点表分段”检测全部点。", remaining, maxImportWindows))
	}
	if len(spans) > 0 {
		lines = append(lines, "按点表新建了读取窗口："+strings.Join(spans, "、")+"。")
		// 一次读取最多 125 个寄存器，点表分散时要建多个窗口；最大化显示一个，寄存器显示得最多，上方标签列出全部窗口
		if len(ws.windows) > 1 {
			ws.mdi.setMaxed(true)
			lines = append(lines, fmt.Sprintf("共 %d 个读取窗口，最大化显示第一个，点读取区上方的标签切换；要同时看几个窗口，用“窗口 → 平铺”或“层叠”。", len(ws.windows)))
		}
	}
	if len(ws.windows) > 0 && !slices.ContainsFunc(ws.windows, func(w *readWindow) bool { return w.def.Slave != ws.windows[0].def.Slave }) {
		lines = append(lines, fmt.Sprintf("读取窗口读的是 Slave %d；设备站号不是 %[1]d 时，点下面的“修改站号”一次改好全部窗口。", ws.windows[0].def.Slave))
	}
	if imp.NoOrder && slices.ContainsFunc(imp.Points, func(p point) bool { return p.Type != typeString && p.Regs() > 1 }) {
		lines = append(lines, "文件里没有字节序，32 / 64 位点先按 ABCD（Telegraf 的默认值）解码；数值不对时读取窗口会提示改用哪种，一键改好。")
	}
	if n := len(imp.Skipped); n > 0 {
		lines = append(lines, fmt.Sprintf("\n跳过 %d 行：", n))
		if imp.Overlaps > 0 {
			lines = append(lines, fmt.Sprintf("其中 %d 行和前面的点地址重叠，只导入了先出现的点。32 位点（REAL、DINT 等）占 2 个寄存器、64 位点占 4 个，"+
				"下一个点的地址要往后隔开这么多；平台按这张表采集时，重叠的点至少有一个读数是错的，请对照设备手册核对属性标识里的地址。", imp.Overlaps))
		}
		lines = append(lines, imp.Skipped[:min(n, 12)]...)
		if n > 12 {
			lines = append(lines, "……")
		}
	}
	if len(imp.Notes) > 0 {
		lines = append(lines, "")
		lines = append(lines, imp.Notes...)
	}
	return strings.Join(lines, "\n")
}

func (ws *Workspace) showImportResult(msg string) {
	l := widget.NewLabel(msg)
	l.Wrapping = fyne.TextWrapWord
	l.Resize(fyne.NewSize(480, 0)) // 自动换行的标签要先定宽度，MinSize 才是换行后的高度
	scroll := container.NewVScroll(l)
	scroll.SetMinSize(fyne.NewSize(480, min(l.MinSize().Height, 360)))
	d := dialog.NewCustomWithoutButtons("导入点表", scroll, ws.win)
	slave := widget.NewButton("修改站号…", func() {
		d.Hide()
		ws.showSlaveDialog(ws.current())
	})
	if len(ws.windows) == 0 {
		slave.Disable()
	}
	d.SetButtons([]fyne.CanvasObject{slave, widget.NewButton("好", d.Hide)})
	d.Show()
}

// defsForPoints 按点表的地址生成读取定义：同一数据区里相近的点合成一个窗口，一次最多读 120 个地址，
// 相隔 20 个地址以上另开窗口。
func defsForPoints(ps []point) []readDef {
	var out []readDef
	for _, r := range points.Ranges(ps) {
		d := defaultDef()
		d.Function = r.Area.ReadFunction()
		d.Start, d.Qty, d.Kind = r.Start, r.Count, kindPoint
		out = append(out, d)
	}
	return out
}

// setPointOrder 把点表里按 from 解码的 32 / 64 位数值点全部改成 to（都用 32 位写法），用于一键改正字节序。
func (ws *Workspace) setPointOrder(from, to modbus.ByteOrder) {
	pts := pointTable{}
	for k, p := range ws.points {
		if p.Type != typeString && p.Regs() > 1 && p.Order == from.For(p.Type) {
			p.Order = to.For(p.Type)
		}
		pts[k] = p
	}
	ws.setPoints(pts)
}

type pointOrderScope byte

const (
	orderAll pointOrderScope = iota
	orderWindow
	orderOne
)

// changePointOrder 把指定范围的多寄存器数值点改为同一种字节序；不清空已读数据。
func (ws *Workspace) changePointOrder(scope pointOrderScope, w *readWindow, off uint16, to modbus.ByteOrder) int {
	if !slices.Contains(modbus.Orders32, to) || (scope != orderAll && !slices.Contains(ws.windows, w)) {
		return 0
	}
	pts := pointTable{}
	changed := 0
	for k, p := range ws.points {
		selected := scope == orderAll
		if w != nil && k.Area == w.def.area() {
			switch scope {
			case orderWindow:
				selected = int(w.def.Start) <= int(k.Offset) && int(k.Offset)+p.Regs() <= int(w.def.Start)+w.def.Qty
			case orderOne:
				selected = k.Offset == off
			}
		}
		if selected && p.Type != typeString && p.Regs() > 1 {
			if next := to.For(p.Type); p.Order != next {
				p.Order = next
				changed++
			}
		}
		pts[k] = p
	}
	if changed > 0 {
		ws.points = pts
		for _, x := range ws.windows {
			x.bar.sync()
			x.refresh()
		}
	}
	return changed
}

// showPointOrderDialog 让用户明确选择字节序和作用范围；菜单与解析面板共用。主窗口上已有对话框时不再弹。
func (ws *Workspace) showPointOrderDialog(w *readWindow) {
	if ws.dialogOpen() {
		ws.win.RequestFocus()
		return
	}
	if w != nil && !w.def.bits() && (w.def.Kind != kindPoint || w.sel >= 0 && w.valueFormat(w.sel).kind != kindPoint || w.sel < 0 && len(w.def.Formats) > 0) {
		w.showByteOrderDialog()
		return
	}
	const all, window, one = "全部点", "本读取窗口的点", "单个点"
	scopes := []string{all}
	selected := all
	if slices.Contains(ws.windows, w) {
		scopes = append(scopes, window)
		selected = window
	}
	var off uint16
	if w != nil && w.sel >= 0 {
		off = w.def.Start + uint16(w.sel)
		if p, ok := ws.points.Get(w.def.area(), off); ok && p.Type != typeString && p.Regs() > 1 {
			scopes = append(scopes, one)
			selected = one
		}
	}
	scope := widget.NewSelect(scopes, nil)
	scope.SetSelected(selected)
	var orders []string
	for _, o := range modbus.Orders32 {
		orders = append(orders, string(o))
	}
	order := widget.NewSelect(orders, nil)
	order.SetSelected(string(modbus.OrderABCD))
	if selected == one {
		p, _ := ws.points.Get(w.def.area(), off)
		order.SetSelected(string(p.Order.For(modbus.TypeFloat32)))
	}
	dialog.NewForm("调整点表字节序", "应用", "取消", []*widget.FormItem{
		widget.NewFormItem("范围", scope), widget.NewFormItem("字节序", order),
	}, func(ok bool) {
		if !ok {
			return
		}
		kind := map[string]pointOrderScope{all: orderAll, window: orderWindow, one: orderOne}[scope.Selected]
		n := ws.changePointOrder(kind, w, off, modbus.ByteOrder(order.Selected))
		dialog.ShowInformation("点表字节序", fmt.Sprintf("已调整 %d 个多寄存器数值点。", n), ws.win)
	}, ws.win).Show()
}

// setSlave 把站号是 from 的读取窗口改成 to（from 为 0 时改全部窗口）。改的是同一台设备的地址，
// 不是换设备，窗口里设的寄存器数据类型保留（读取定义里改 Slave ID 会清掉）。
func (ws *Workspace) setSlave(from, to byte) {
	changed := false
	for _, w := range ws.windows {
		if w.def.Slave == to || from != 0 && w.def.Slave != from {
			continue
		}
		d := w.def
		d.Slave = to
		w.halt()
		w.setDef(d)
		w.start()
		changed = true
	}
	if changed {
		ws.relayout()
	}
}

// showSlaveDialog 一键修改站号：点表对应一台设备，导入后按地址建了好几个读取窗口，不用逐个打开读取定义改。
// 窗口用了几个站号时（同一总线上的几台设备）先选改哪一台，默认是 w 的站号。
func (ws *Workspace) showSlaveDialog(w *readWindow) {
	if len(ws.windows) == 0 {
		dialog.ShowInformation("修改站号", "还没有读取窗口。", ws.win)
		return
	}
	count := map[byte]int{}
	for _, x := range ws.windows {
		count[x.def.Slave]++
	}
	ids := slices.Sorted(maps.Keys(count))
	cur := ws.windows[0].def.Slave
	if slices.Contains(ws.windows, w) {
		cur = w.def.Slave
	}
	label := func(id byte) string { return fmt.Sprintf("Slave %d 的读取窗口（%d 个）", id, count[id]) }
	all := fmt.Sprintf("全部读取窗口（%d 个）", len(ws.windows))
	var scopes []string
	for _, id := range ids {
		scopes = append(scopes, label(id))
	}
	slave := widget.NewEntry()
	slave.SetText(strconv.Itoa(int(cur)))
	slave.Validator = slaveValidator
	scope := widget.NewSelect(append(scopes, all), func(s string) {
		for _, id := range ids {
			if s == label(id) {
				slave.SetText(strconv.Itoa(int(id))) // 从这组窗口现在的站号改起
			}
		}
	})
	scope.SetSelected(label(cur))
	var scopeItem fyne.CanvasObject = scope
	if len(ids) == 1 {
		scopeItem = widget.NewLabel(fmt.Sprintf("全部 %d 个读取窗口，现在是 Slave %d", len(ws.windows), cur))
	}
	dialog.NewForm("修改站号", "应用", "取消", []*widget.FormItem{
		widget.NewFormItem("范围", scopeItem), widget.NewFormItem("新 Slave ID", slave),
	}, func(ok bool) {
		if !ok {
			return
		}
		to, _ := strconv.Atoi(strings.TrimSpace(slave.Text))
		from := byte(0)
		for _, id := range ids {
			if scope.Selected == label(id) {
				from = id
			}
		}
		ws.setSlave(from, byte(to))
	}, ws.win).Show()
	ws.win.Canvas().Focus(slave)
}

func slaveValidator(s string) error {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 1 || v > 255 {
		return errors.New("Slave ID 应为 1–255")
	}
	return nil
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
