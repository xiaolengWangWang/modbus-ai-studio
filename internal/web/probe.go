package web

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/eta"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
	"modbus-ai-studio/internal/probe"
	"modbus-ai-studio/internal/transport"
)

// 检测寄存器：和桌面版、modbus-cli 同一套算法（internal/probe）。同一时刻只做一个检测，在后台跑，
// 页面从推送的状态里看进度，可以停止。检测期间读取表暂停；网络设备让出常规连接，用独立的检测连接
// （断线自己重连，Modbus TCP 超时后换新连接），检测完马上恢复常规连接。串口共用常规连接。

var probeModes = map[string]probe.Mode{"auto": probe.ModeAuto, "single": probe.ModeSingle, "points": probe.ModePoints,
	"each": probe.ModeEachPoint, "quick": probe.ModeQuick}

var probeTitles = map[probe.Mode]string{probe.ModeAuto: "自动定位", probe.ModeSingle: "逐个寄存器", probe.ModePoints: "按点表分段",
	probe.ModeEachPoint: "逐点独立验证", probe.ModeQuick: "快速排查"}

// probeRequest 是开始检测：auto、single、quick 检测 address 起 count 个；points、each 按读取表里的点表。
type probeRequest struct {
	How      string `json:"how"`
	Slave    int    `json:"slave"`
	Function int    `json:"function"`
	Address  string `json:"address"`
	Count    int    `json:"count"`
}

// probeBrief 是推送状态里的检测进度；结束后页面再取 /api/probe 看完整结果。
type probeBrief struct {
	ID       int    `json:"id"`
	Running  bool   `json:"running"`
	How      string `json:"how"`
	Title    string `json:"title"`
	Progress string `json:"progress"`
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Note     string `json:"note,omitempty"` // 正在复核、等待重连
	Summary  string `json:"summary,omitempty"`
	Problems bool   `json:"problems"`
	Error    string `json:"error,omitempty"`
}

// probeSpan 是常规读取可以改用的范围。
type probeSpan struct {
	Address string `json:"address"`
	Count   int    `json:"count"`
}

// probeView 是检测的完整结果。
type probeView struct {
	probeBrief
	Slave     int        `json:"slave"`
	Function  int        `json:"function"`
	Text      string     `json:"text"`
	Readable  *probeSpan `json:"readable,omitempty"`
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	Requests  int        `json:"requests"`
	ElapsedMS int64      `json:"elapsedMs"`
}

type probeJob struct {
	id     int
	how    string
	unit   string
	cancel context.CancelFunc
	run    *probe.Run

	mu       sync.Mutex
	done     int
	total    int
	note     string
	finished bool
	out      probe.Output
	result   probeView
}

func (j *probeJob) brief() probeBrief {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return j.result.probeBrief
	}
	d := j.run.Def
	b := probeBrief{ID: j.id, Running: true, How: j.how, Done: j.done, Total: j.total, Note: j.note,
		Title: fmt.Sprintf("%s · Slave %d · %s · %s", probeTitles[j.run.Mode], d.Slave, d.Function, modbus.ReferenceSpan(modbus.AreaOf(d.Function), d.Start, d.Qty))}
	b.Progress = fmt.Sprintf(j.unit, j.done, j.total) + " · " + eta.Text(float64(j.done)/float64(max(j.total, 1)), j.run.Started)
	return b
}

// probing 表示正在检测：读取表暂停。
func (s *Server) probing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probe != nil && !s.probe.isFinished()
}

func (j *probeJob) isFinished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished
}

// probeFrame 把收发的报文记进正在进行的检测，导出报告用。
func (s *Server) probeFrame(p modbus.Packet) {
	if l := s.probeLog.Load(); l != nil {
		l.Frame(p)
	}
}

// probePoints 是读取表里的点表：导入点表建的读取表才有。
func (s *Server) probePoints() points.Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ps []points.Point
	for _, t := range s.tables {
		if len(t.cfg.Points) > 0 {
			ps = append(ps, t.points...)
		}
	}
	return points.NewTable(ps)
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var in probeRequest
	if !readJSON(w, r, &in) {
		return
	}
	how, ok := probeModes[in.How]
	if !ok {
		writeError(w, http.StatusBadRequest, "检测方式应为 auto、single、quick、points 或 each")
		return
	}
	fn := modbus.FunctionCode(in.Function)
	if !fn.IsRead() {
		writeError(w, http.StatusBadRequest, "功能码应为 01、02、03 或 04")
		return
	}
	if in.Slave < 1 || in.Slave > 255 {
		writeError(w, http.StatusBadRequest, "Slave ID 应为 1–255（0 是广播，没有读响应）")
		return
	}
	d := probe.Def{Slave: byte(in.Slave), Function: fn}
	pts := s.probePoints()
	var segs []probe.Segment
	if how.UsesPoints() {
		var ok bool
		if d, segs, ok = probe.PointDef(pts, d); !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("读取表里没有 %s 能读的点：先导入点表，或换功能码", fn))
			return
		}
	} else {
		start, err := resolveAddress(in.Address, modbus.AreaOf(fn))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		d.Start, d.Qty = start, in.Count
	}
	if err := probe.Validate(d); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.probeStart.Lock()
	defer s.probeStart.Unlock()
	if s.probing() {
		writeError(w, http.StatusConflict, "已经在检测，等它结束或先停止")
		return
	}
	client, release, err := s.dev.probeClient(s.probeFrame)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	j := &probeJob{how: in.How, cancel: cancel}
	j.total, j.unit = probeTotal(how, d, segs)
	j.run = &probe.Run{Mode: how, Def: d, Target: s.dev.desc(), Timeout: s.dev.timeout(), Started: time.Now(), Log: probe.NewLog(),
		Recorded: s.opts.Recorder != nil}
	for _, sg := range segs {
		j.run.Points = append(j.run.Points, sg.Points...)
	}
	s.mu.Lock()
	s.probeID++
	j.id = s.probeID
	s.probe = j
	s.mu.Unlock()
	s.probeLog.Store(j.run.Log)
	s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "PROBE", Detail: j.brief().Title + "：开始，读取表暂停"})
	s.changed()
	s.wg.Go(func() {
		defer cancel()
		s.runProbe(ctx, j, client, pts, segs)
		release()
		s.probeLog.CompareAndSwap(j.run.Log, nil)
		s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "PROBE", Detail: j.result.Title + "：" + firstNonEmpty(j.result.Error, j.result.Summary)})
		s.changed()
	})
	writeJSON(w, http.StatusAccepted, j.brief())
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// runProbe 在后台检测，结束后填好结果。
func (s *Server) runProbe(ctx context.Context, j *probeJob, c probe.Client, pts points.Table, segs []probe.Segment) {
	d, how := j.run.Def, j.run.Mode
	progress := func(n int) {
		j.mu.Lock()
		j.done = n
		j.mu.Unlock()
		s.changed()
	}
	client := probe.Logged{Client: c, Log: j.run.Log}
	var res []int8
	var checks []probe.PointCheck
	var quick *probe.QuickResult
	var stopErr error
	switch how {
	case probe.ModeAuto:
		res, stopErr = probe.Locate(ctx, client, d, progress, func(offset int) {
			j.mu.Lock()
			j.note = "初检完成，正在复核未响应地址 · " + modbus.Reference(modbus.AreaOf(d.Function), d.Start+uint16(offset))
			j.mu.Unlock()
			s.changed()
		})
	case probe.ModeEachPoint:
		checks, stopErr = probe.CheckEachPoint(ctx, client, d, segs, progress)
	case probe.ModeQuick:
		quick, stopErr = probe.QuickCheck(ctx, client, d, pts, probe.QuickBudget, progress)
	default:
		res, stopErr = probe.Units(ctx, client, d, how, segs, progress)
	}
	if ctx.Err() != nil && stopErr == nil {
		stopErr = context.Canceled
	}
	run := j.run
	run.Finished, run.StopErr = time.Now(), stopErr
	if quick != nil {
		for _, u := range quick.Units {
			if u.P != nil {
				run.Points = append(run.Points, *u.P)
			}
		}
	}
	var out probe.Output
	bestStart, bestN := 0, 0
	switch how {
	case probe.ModeEachPoint:
		out = probe.EachPointOutput(d, checks, segs, stopErr)
	case probe.ModeQuick:
		out = probe.QuickText(d, quick, stopErr)
		out.Text += "\n\n" + probe.IllegalNote
	default:
		out, bestStart, bestN = probe.OutputFor(d, res, stopErr, segs...)
	}
	b := j.brief()
	b.Running, b.Note, b.Summary, b.Problems = false, "", out.Summary, out.Warn
	b.Progress = fmt.Sprintf(j.unit, b.Done, b.Total) + " · 用时 " + eta.Rough(run.Finished.Sub(run.Started))
	switch {
	case errors.Is(stopErr, context.Canceled):
		b.Error = "检测已停止，保留已完成的结果"
	case stopErr != nil:
		b.Error = "检测没有做完：" + stopErr.Error()
	}
	v := probeView{probeBrief: b, Slave: int(d.Slave), Function: int(d.Function), Text: out.Text, Columns: out.CSVHeader, Rows: out.CSVRows,
		Requests: len(run.Log.Snapshot()), ElapsedMS: run.Finished.Sub(run.Started).Milliseconds()}
	if v.Rows == nil {
		v.Rows = [][]string{}
	}
	if bestN > 0 && (how == probe.ModeAuto || how == probe.ModeSingle) {
		v.Readable = &probeSpan{Address: modbus.Reference(modbus.AreaOf(d.Function), d.Start+uint16(bestStart)), Count: bestN}
	}
	j.mu.Lock()
	j.finished, j.out, j.result = true, out, v
	j.mu.Unlock()
}

// probeTotal 是进度的总数和写法，和桌面版一样。
func probeTotal(how probe.Mode, d probe.Def, segs []probe.Segment) (int, string) {
	switch how {
	case probe.ModePoints:
		total := 0
		for _, sg := range segs {
			total += sg.N
		}
		return total, "已检测 %d / %d"
	case probe.ModeEachPoint:
		total := len(segs)
		for _, sg := range segs {
			total += len(sg.Points)
		}
		return total, "已发请求 %d / %d（先整段，后逐点）"
	case probe.ModeQuick:
		batch := modbus.MaxReadRegisters
		if d.Function == modbus.FuncReadCoils || d.Function == modbus.FuncReadDiscreteInputs {
			batch = modbus.MaxReadBits
		}
		return (d.Qty+batch-1)/batch + probe.QuickBudget, "已发请求 %d / 最多 %d（先批量，后抽查）"
	}
	return d.Qty, "已检测 %d / %d"
}

func (s *Server) handleProbeResult(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.probe
	s.mu.Unlock()
	if j == nil {
		writeError(w, http.StatusNotFound, "还没有检测过")
		return
	}
	j.mu.Lock()
	finished, v := j.finished, j.result
	j.mu.Unlock()
	if !finished {
		v = probeView{probeBrief: j.brief(), Columns: []string{}, Rows: [][]string{}}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleProbeStop(w http.ResponseWriter, r *http.Request) {
	s.stopProbe()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// stopProbe 停止正在进行的检测，保留已完成的结果。
func (s *Server) stopProbe() {
	s.mu.Lock()
	j := s.probe
	s.mu.Unlock()
	if j != nil {
		j.cancel()
	}
}

// handleProbeReport 下载最近一次检测的报告：Markdown 摘要、JSONL 请求明细和 CSV 失败清单打成一个 zip。
// note 是实际设备的备注（例如经 DTU 转发时设备真正的地址），只写进报告。
func (s *Server) handleProbeReport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.probe
	s.mu.Unlock()
	if j == nil || !j.isFinished() {
		writeError(w, http.StatusNotFound, "没有做完的检测")
		return
	}
	dir, err := os.MkdirTemp("", "modbus-probe-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	d := j.run.Def
	base := fmt.Sprintf("检测报告-Slave%d-FC%02d-%s", d.Slave, byte(d.Function), j.run.Started.Format("20060102-150405"))
	j.mu.Lock()
	out := j.out
	j.mu.Unlock()
	note := strings.TrimSpace(r.URL.Query().Get("note"))
	files, err := probe.ExportReport(r.Context(), j.run, out, note, filepath.Join(dir, base+".md"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "检测报告没有完整生成："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".zip"}))
	zw := zip.NewWriter(w)
	for _, f := range files {
		if err := addZipFile(zw, f); err != nil {
			return // 已经开始发送，只能中断
		}
	}
	zw.Close()
}

func addZipFile(zw *zip.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	h.Name, h.Method = filepath.Base(path), zip.Deflate
	dst, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, f)
	return err
}

// ---- 检测用的连接 ----

// probeClient 是检测用的连接，release 在检测结束后调用。网络设备让出常规连接，另建独立的检测连接；
// 串口共用常规连接。没连上设备时报错。
func (d *device) probeClient(observe func(modbus.Packet)) (probe.Client, func(), error) {
	d.mu.Lock()
	if d.state != stateConnected || d.client == nil {
		d.mu.Unlock()
		return nil, nil, errors.New("先连接设备，再检测寄存器")
	}
	mode, target, session := d.mode, d.target, d.session
	timeout := time.Duration(d.cfg.TimeoutMs) * time.Millisecond
	if mode.Serial() {
		d.mu.Unlock()
		return serialProbeClient{d}, func() {}, nil
	}
	main := d.client
	d.client, d.state, d.since = nil, stateProbing, time.Now()
	d.mu.Unlock()
	main.Close() // 有的设备只接受一条连接，检测期间让出来；在途的读取返回连接错误，linkDown 认得出不是当前连接
	d.s.changed()
	opts := modbus.DefaultOptions(mode)
	opts.Timeout, opts.ReadRetries = timeout, 0
	opts.Observer = modbus.ObserverFunc(func(p modbus.Packet) {
		if r := d.rec(); r != nil && session != 0 {
			r.Record(session, p)
		}
		d.s.packets.add(p)
		observe(p)
	})
	c := &probe.NetClient{Options: opts, Dial: func(ctx context.Context) (modbus.Transport, error) {
		return transport.DialTCP(ctx, target, max(timeout, 3*time.Second))
	}}
	return c, func() {
		c.Close()
		d.resume()
	}, nil
}

// resume 检测结束后恢复常规连接：马上重连。期间用户断开或换了连接就不管。
func (d *device) resume() {
	d.mu.Lock()
	if d.state != stateProbing {
		d.mu.Unlock()
		return
	}
	d.state, d.since = stateReconnecting, time.Now()
	ctx := d.ctx
	d.mu.Unlock()
	d.s.changed()
	go d.reconnect(ctx, 0)
}

// desc 是检测报告里写的连接入口。
func (d *device) desc() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(d.mode) + " " + d.cfg.describe(d.target)
}

func (d *device) timeout() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Duration(d.cfg.TimeoutMs) * time.Millisecond
}

// serialProbeClient 用串口的常规连接检测；读取表在检测期间暂停，不会和检测抢串口。
type serialProbeClient struct{ d *device }

func (c serialProbeClient) Mode() modbus.Mode {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	return c.d.mode
}

func (c serialProbeClient) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
	client := c.d.current()
	if client == nil {
		return nil, fmt.Errorf("串口已断开，请重新连接后检测：%w", modbus.ErrConnection)
	}
	resp, err := client.Do(ctx, req)
	if errors.Is(err, modbus.ErrConnection) && ctx.Err() == nil {
		c.d.linkDown(client, err)
	}
	return resp, err
}
