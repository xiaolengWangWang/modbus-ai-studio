package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/eta"
	"modbus-ai-studio/internal/slavescan"
)

// 搜索站号：和桌面版、modbus-cli 同一套算法（internal/slavescan），每个站号依次读用户给的地址、40001、30001、线圈 1。
// 和检测寄存器一样同一时刻只做一个、在后台跑、可以停止，读取表暂停；进度和找到的站号跟着状态推送。
// 用常规连接发请求：大部分站号本来就没有设备，要是像检测那样 Modbus TCP 超时就换连接，会建几百次连接；
// Modbus TCP 有事务号，晚到的响应认得出来。

// scanRequest 是开始搜索：站号 from–to，每次读取最多等 timeoutMs，address 是先读的地址（设备上肯定有的点，可空）。
type scanRequest struct {
	From      int    `json:"from"`
	To        int    `json:"to"`
	TimeoutMS int    `json:"timeoutMs"`
	Address   string `json:"address"`
}

type scanHitView struct {
	Slave int     `json:"slave"`
	Probe string  `json:"probe"` // 哪次读取得到的应答，例如 40001（FC03）
	RTTMS float64 `json:"rttMs"`
	Note  string  `json:"note"`
}

// scanView 是推送状态里的搜索：进行中是进度，结束后是结果。
type scanView struct {
	ID       int           `json:"id"`
	Running  bool          `json:"running"`
	From     int           `json:"from"`
	To       int           `json:"to"`
	Probes   []string      `json:"probes"` // 每个站号依次试的读取
	Done     int           `json:"done"`   // 已搜完的站号个数
	Total    int           `json:"total"`
	Progress string        `json:"progress"`
	Found    []scanHitView `json:"found"`
	Stopped  bool          `json:"stopped,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type scanJob struct {
	cancel  context.CancelFunc
	started time.Time
	waiting bool // 在等读取表在途的请求结束

	mu   sync.Mutex
	view scanView
}

func (j *scanJob) snapshot() scanView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.view
	v.Found = append([]scanHitView{}, v.Found...)
	if v.Running && j.waiting {
		v.Progress = "等读取表正在发的请求结束（读取表用的站号不对时要等到超时）…"
	} else if v.Running {
		v.Progress = fmt.Sprintf("已搜 %d / %d 个站号 · %s", v.Done, v.Total, eta.Text(float64(v.Done)/float64(max(v.Total, 1)), j.started))
	}
	return v
}

// scanning 表示正在搜索站号：读取表暂停。
func (s *Server) scanning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scan == nil {
		return false
	}
	s.scan.mu.Lock()
	defer s.scan.mu.Unlock()
	return s.scan.view.Running
}

// busy 表示正在检测寄存器或搜索站号，这时不能再开始另一个。
func (s *Server) busy() bool { return s.probing() || s.scanning() }

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var in scanRequest
	if !readJSON(w, r, &in) {
		return
	}
	if in.From == 0 && in.To == 0 {
		in.From, in.To = 1, 247
	}
	if in.TimeoutMS == 0 {
		in.TimeoutMS = 200
	}
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	if err := slavescan.Validate(in.From, in.To, timeout); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var extra []slavescan.Probe
	if a := strings.TrimSpace(in.Address); a != "" {
		p, err := slavescan.ParseProbe(a)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("地址 %s：%v", a, err))
			return
		}
		extra = append(extra, p)
	}
	probes := slavescan.Probes(extra...)

	s.probeStart.Lock()
	defer s.probeStart.Unlock()
	if s.busy() {
		writeError(w, http.StatusConflict, "正在检测寄存器或搜索站号，等它结束或先停止")
		return
	}
	if s.dev.current() == nil {
		writeError(w, http.StatusConflict, "先连接设备，再搜索站号")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	j := &scanJob{cancel: cancel, started: time.Now(), waiting: true}
	j.view = scanView{Running: true, From: in.From, To: in.To, Total: in.To - in.From + 1, Found: []scanHitView{}}
	for _, p := range probes {
		j.view.Probes = append(j.view.Probes, p.String())
	}
	s.mu.Lock()
	s.scanID++
	j.view.ID = s.scanID
	s.scan = j
	s.mu.Unlock()
	s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "SCAN", Detail: fmt.Sprintf("Slave %d–%d：开始，读取表暂停", in.From, in.To)})
	s.changed()
	s.wg.Go(func() {
		defer cancel()
		s.pollGate.Lock()
		j.mu.Lock()
		j.started, j.waiting = time.Now(), false // 预计时间从真正开始发请求算
		j.mu.Unlock()
		hits, err := slavescan.Scan(ctx, sharedClient{s.dev}, in.From, in.To, timeout, probes, func(id int, hits []slavescan.Hit) {
			j.mu.Lock()
			j.view.Done, j.view.Found = id-in.From+1, hitViews(hits)
			j.mu.Unlock()
			s.changed()
		})
		j.mu.Lock()
		v := &j.view
		v.Running, v.Found, v.Stopped = false, hitViews(hits), ctx.Err() != nil
		v.Progress = fmt.Sprintf("搜了 %d / %d 个站号 · 用时 %s", v.Done, v.Total, eta.Rough(time.Since(j.started)))
		if err != nil {
			v.Error = "搜索中途连接断开：" + err.Error()
		}
		detail := scanSummary(*v)
		j.mu.Unlock()
		s.pollGate.Unlock()
		s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "SCAN", Detail: detail})
		s.changed()
	})
	writeJSON(w, http.StatusAccepted, j.snapshot())
}

func hitViews(hits []slavescan.Hit) []scanHitView {
	out := []scanHitView{}
	for _, h := range hits {
		out = append(out, scanHitView{Slave: h.ID, Probe: h.Probe.String(), RTTMS: float64(h.RTT.Microseconds()) / 1000, Note: h.Note()})
	}
	return out
}

// scanSummary 是日志里的一行结论。
func scanSummary(v scanView) string {
	var ids []string
	for _, h := range v.Found {
		ids = append(ids, strconv.Itoa(h.Slave))
	}
	found := "没有设备应答"
	if len(ids) > 0 {
		found = "找到 Slave " + strings.Join(ids, "、")
	}
	switch {
	case v.Error != "":
		return fmt.Sprintf("Slave %d–%d：%s；%s", v.From, v.To, v.Error, found)
	case v.Stopped:
		return fmt.Sprintf("Slave %d–%d：已停止，%s", v.From, v.To, found)
	}
	return fmt.Sprintf("Slave %d–%d：%s", v.From, v.To, found)
}

func (s *Server) handleScanStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.scan
	s.mu.Unlock()
	if j != nil {
		j.cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
