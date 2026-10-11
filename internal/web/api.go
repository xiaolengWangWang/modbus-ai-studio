package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/control"
	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

const maxTables = 32

func baseName(path string) string { return filepath.Base(path) }

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	var cfg ConnConfig
	if !readJSON(w, r, &cfg) {
		return
	}
	if err := s.dev.connect(cfg); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.dev.view())
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	s.dev.disconnect()
	writeJSON(w, http.StatusOK, s.dev.view())
}

// handleDemo 连接内置换热站模拟器，读取表换成示例点表，用来熟悉界面。
func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	if err := s.dev.connect(ConnConfig{Mode: "tcp", Simulator: true}); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.mu.Lock()
	old := s.tables
	s.tables = nil
	s.mu.Unlock()
	for _, t := range old {
		t.stop()
	}
	for _, cfg := range demoTables() {
		if _, err := s.addTable(cfg); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.state())
}

func demoTables() []TableConfig {
	group := func(name string, start uint16, count int) TableConfig {
		cfg := TableConfig{Name: name, Slave: 1, Function: 3, Address: strconv.Itoa(int(start)), Count: count, IntervalMs: 1000}
		for _, p := range simulator.HeatStationPoints {
			if p.Offset >= start && int(p.Offset) < int(start)+count {
				cfg.Points = append(cfg.Points, PointConfig{Offset: p.Offset, Name: p.Name, Type: string(p.Type), Order: string(p.Order), Scale: p.Scale, Unit: p.Unit, Writable: p.RW})
			}
		}
		return cfg
	}
	return []TableConfig{group("换热站实时数据", 0, 20), group("设定值", 346, 8), group("气象站", 600, 4)}
}

func (s *Server) addTable(cfg TableConfig) (*table, error) {
	t, err := newTable(cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.tables) >= maxTables {
		s.mu.Unlock()
		return nil, fmt.Errorf("最多 %d 个读取表", maxTables)
	}
	s.tableID++
	t.id = s.tableID
	s.tables = append(s.tables, t)
	s.mu.Unlock()
	s.startTable(t)
	s.changed()
	return t, nil
}

func (s *Server) findTable(r *http.Request) (int, *table) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return -1, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tables {
		if t.id == id {
			return i, t
		}
	}
	return -1, nil
}

func (s *Server) handleAddTable(w http.ResponseWriter, r *http.Request) {
	var cfg TableConfig
	if !readJSON(w, r, &cfg) {
		return
	}
	t, err := s.addTable(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t.view())
}

func (s *Server) handleUpdateTable(w http.ResponseWriter, r *http.Request) {
	var cfg TableConfig
	if !readJSON(w, r, &cfg) {
		return
	}
	_, old := s.findTable(r)
	if old == nil {
		writeError(w, http.StatusNotFound, "读取表不存在")
		return
	}
	t, err := newTable(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	old.stop()
	old.mu.Lock()
	t.id, t.paused = old.id, old.paused
	old.mu.Unlock()
	s.mu.Lock()
	if i := slices.Index(s.tables, old); i >= 0 {
		s.tables[i] = t
	}
	s.mu.Unlock()
	s.startTable(t)
	s.changed()
	writeJSON(w, http.StatusOK, t.view())
}

func (s *Server) handleDeleteTable(w http.ResponseWriter, r *http.Request) {
	_, t := s.findTable(r)
	if t == nil {
		writeError(w, http.StatusNotFound, "读取表不存在")
		return
	}
	t.stop()
	s.mu.Lock()
	s.tables = slices.DeleteFunc(s.tables, func(x *table) bool { return x == t })
	s.mu.Unlock()
	s.changed()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePauseTable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	_, t := s.findTable(r)
	if t == nil {
		writeError(w, http.StatusNotFound, "读取表不存在")
		return
	}
	t.setPaused(in.Paused)
	s.changed()
	writeJSON(w, http.StatusOK, t.view())
}

func (s *Server) handlePauseAll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.mu.Lock()
	tables := slices.Clone(s.tables)
	s.mu.Unlock()
	for _, t := range tables {
		t.setPaused(in.Paused)
	}
	s.changed()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (t *table) setPaused(paused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = paused
	if paused {
		t.status = "paused"
	} else if t.status == "paused" {
		t.status = "waiting"
	}
}

// ---- 写入 ----

type writeRequest struct {
	Slave    int     `json:"slave"`
	Coil     bool    `json:"coil"`
	Address  string  `json:"address"`
	Type     string  `json:"type"`
	Order    string  `json:"order"`
	Scale    float64 `json:"scale"`
	Value    string  `json:"value"`
	Function int     `json:"function"` // 0 自动：单个用 FC06 / FC05，多个用 FC16
}

type readbackView struct {
	AtMs  int64  `json:"atMs"`
	Regs  string `json:"regs"`
	Value string `json:"value"`
	Error string `json:"error,omitempty"`
}

type writeResult struct {
	Result     string         `json:"result"`
	Text       string         `json:"text"`
	Hint       string         `json:"hint,omitempty"`
	Rounded    bool           `json:"rounded"`
	Written    string         `json:"written"`
	Original   string         `json:"original"`
	Target     string         `json:"target"`
	WriteError string         `json:"writeError,omitempty"`
	Readbacks  []readbackView `json:"readbacks"`
}

var resultText = map[control.Result]string{
	control.ResultPass:          "通过：回读与目标一致",
	control.ResultNotApplied:    "未生效：回读一直是原值",
	control.ResultOverwritten:   "被改回：先变成目标值，后又变回",
	control.ResultMismatch:      "值不符：变了但不等于目标值",
	control.ResultUnknown:       "无法判断：写入超时，回读也看不出是否生效",
	control.ResultProtocolError: "设备拒绝或读原值失败",
}

// handleWrite 写入后按 0 ms、200 ms、1000 ms 回读，判断是否真正生效。
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	var in writeRequest
	if !readJSON(w, r, &in) {
		return
	}
	c := s.dev.current()
	if c == nil {
		writeError(w, http.StatusConflict, "未连接设备")
		return
	}
	if in.Slave < 0 || in.Slave > 255 {
		writeError(w, http.StatusBadRequest, "Slave ID 应为 0–255")
		return
	}
	area, dt, order := modbus.AreaHoldingRegisters, modbus.TypeUint16, modbus.OrderAB
	allowed := []int{0, 6, 16}
	if in.Coil {
		area, allowed = modbus.AreaCoils, []int{0, 5, 15}
	} else {
		var err error
		if dt, order, err = parseType(in.Type, in.Order); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !slices.Contains(allowed, in.Function) || in.Function == 6 && dt.Registers() > 1 {
		writeError(w, http.StatusBadRequest, "功能码与写入对象不符：寄存器用 06 / 16（多个寄存器只能 16），线圈用 05 / 15")
		return
	}
	off, err := resolveAddress(in.Address, area)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scale := in.Scale
	if scale == 0 {
		scale = 1
	}
	target := control.Target{Slave: byte(in.Slave), Area: area, Address: off, Type: dt, ReadOrder: order,
		Scaling: modbus.Scaling{Scale: scale}, Function: modbus.FunctionCode(in.Function)}
	regs, _, rounded, err := control.EncodeText(target, in.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rep := control.Verify(ctx, c, target, regs, control.DefaultSchedule)
	if errors.Is(rep.WriteErr, modbus.ErrConnection) {
		s.dev.linkDown(c, rep.WriteErr)
	}
	show := func(regs []uint16) string {
		if len(regs) == 0 {
			return "—"
		}
		if in.Coil {
			return strconv.Itoa(int(regs[0]))
		}
		v, _ := formatValue(dt, order, scale, regs)
		return v
	}
	out := writeResult{Result: string(rep.Result), Text: resultText[rep.Result], Hint: rep.Hint, Rounded: rounded,
		Written: hexRegs(rep.Written), Original: show(rep.OriginalRegs), Target: show(regs), Readbacks: []readbackView{}}
	if rep.WriteErr != nil {
		out.WriteError = explain(rep.WriteErr)
	}
	for _, rb := range rep.Readbacks {
		v := readbackView{AtMs: rb.At.Milliseconds(), Regs: hexRegs(rb.Registers), Value: show(rb.Registers)}
		if rb.Err != nil {
			v.Error = explain(rb.Err)
		}
		out.Readbacks = append(out.Readbacks, v)
	}
	ref := modbus.Reference(area, off)
	s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "WRITE", Detail: fmt.Sprintf("Slave %d %s = %s：%s", in.Slave, ref, strings.TrimSpace(in.Value), out.Text)})
	s.changed()
	writeJSON(w, http.StatusOK, out)
}

func hexRegs(regs []uint16) string {
	parts := make([]string, len(regs))
	for i, r := range regs {
		parts[i] = fmt.Sprintf("%04X", r)
	}
	return strings.Join(parts, " ")
}

// ---- 协议识别、串口、记录文件 ----

func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target    string `json:"target"`
		TimeoutMs int    `json:"timeoutMs"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if _, _, err := net.SplitHostPort(in.Target); err != nil {
		writeError(w, http.StatusBadRequest, "目标地址应为 IP:端口，例如 192.168.1.10:502")
		return
	}
	timeout := time.Duration(max(in.TimeoutMs, 300)) * time.Millisecond
	dial := func(ctx context.Context) (modbus.Transport, error) { return transport.DialTCP(ctx, in.Target, timeout) }
	// 与桌面版相同：有读取表时先试第一个表的 Slave ID，再试常见的 1、255
	slaves := []byte{1, 255}
	s.mu.Lock()
	if len(s.tables) > 0 {
		if id := s.tables[0].cfg.Slave; id != 0 && id != 1 && id != 255 {
			slaves = []byte{byte(id), 1, 255}
		}
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := detect.Detect(ctx, dial, detect.Options{Slaves: slaves, Timeout: timeout, Observer: modbus.ObserverFunc(s.packets.add)})
	type attempt struct {
		Mode   string `json:"mode"`
		Slave  int    `json:"slave"`
		Result string `json:"result"`
	}
	out := struct {
		OK          bool      `json:"ok"`
		Mode        string    `json:"mode,omitempty"` // 连接参数里的写法，例如 rtu-over-tcp
		Name        string    `json:"name,omitempty"`
		Slave       int       `json:"slave,omitempty"`
		ByException bool      `json:"byException"`
		Error       string    `json:"error,omitempty"`
		Attempts    []attempt `json:"attempts"`
	}{OK: err == nil, Attempts: []attempt{}}
	for _, a := range res.Attempts {
		result := "有响应"
		if a.Err != nil {
			result = a.Err.Error()
		}
		out.Attempts = append(out.Attempts, attempt{string(a.Mode), int(a.Slave), result})
	}
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Name, out.Slave, out.ByException = string(res.Mode), int(res.Slave), res.ByException
		for k, m := range modes {
			if m == res.Mode {
				out.Mode = k
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSerialPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := transport.ListSerialPorts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "列出串口失败："+err.Error())
		return
	}
	if ports == nil {
		ports = []string{}
	}
	writeJSON(w, http.StatusOK, ports)
}

type fileView struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Current  bool   `json:"current"`
}

func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request) {
	rec := s.opts.Recorder
	out := struct {
		Enabled bool       `json:"enabled"`
		Dir     string     `json:"dir"`
		Files   []fileView `json:"files"`
	}{Files: []fileView{}}
	if rec == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	files, err := rec.Files()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Enabled, out.Dir = true, filepath.Dir(rec.Path())
	current := rec.Path()
	for i := len(files) - 1; i >= 0; i-- { // 新的在前
		fi, err := os.Stat(files[i])
		if err != nil {
			continue
		}
		size, mod := fi.Size(), fi.ModTime()
		// 正在写的文件，新内容先在 -wal 里，要算上才是下载到的大小和最后写入时间
		if wal, err := os.Stat(files[i] + "-wal"); err == nil {
			size += wal.Size()
			if wal.ModTime().After(mod) {
				mod = wal.ModTime()
			}
		}
		out.Files = append(out.Files, fileView{baseName(files[i]), size, mod.Format("2006-01-02 15:04"), files[i] == current})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDownload 下载一个记录文件。正在写的文件也能下：先做一份包含 WAL 内容的完整快照。
// 只认 Files 列出的文件名，不接受路径。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	rec := s.opts.Recorder
	name := r.PathValue("name")
	if rec == nil {
		writeError(w, http.StatusNotFound, "没有记录文件")
		return
	}
	files, err := rec.Files()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := slices.IndexFunc(files, func(f string) bool { return baseName(f) == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, "没有这个记录文件")
		return
	}
	dir, err := os.MkdirTemp("", "modbus-ai-web-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	snap := filepath.Join(dir, name)
	if err := recorder.Snapshot(files[i], snap); err != nil {
		writeError(w, http.StatusInternalServerError, "导出记录失败："+err.Error())
		return
	}
	f, err := os.Open(snap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	io.Copy(w, f)
}
