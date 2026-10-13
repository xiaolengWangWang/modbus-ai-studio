package probe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/diagnose"
	"modbus-ai-studio/internal/modbus"
)

// 检测请求的类别，记在每条请求上，报告按它分口径统计。
const (
	PhaseBatch     = "batch"     // 批量读取（整段）
	PhasePoint     = "point"     // 逐点单独读取，按点的宽度
	PhaseWord      = "word"      // 逐个寄存器单独读取
	PhaseCandidate = "candidate" // 快速排查：候选点单独读取
	PhaseControl   = "control"   // 快速排查：控制点单独读取
	PhaseBoundary  = "boundary"  // 快速排查：边界对照
)

var phaseNames = map[string]string{PhaseBatch: "批量读取", PhasePoint: "逐点单独读取", PhaseWord: "逐个寄存器",
	PhaseCandidate: "候选点单独读取", PhaseControl: "控制点单独读取", PhaseBoundary: "边界对照"}

type probePhaseKey struct{}

// withPhase 标明接下来的请求属于哪一类读取。
func withPhase(ctx context.Context, phase string) context.Context {
	return context.WithValue(ctx, probePhaseKey{}, phase)
}

// probeEntry 是检测发出的一条请求：请求参数、结果，以及这条请求等待期间收到的全部原始帧（含晚到、不匹配的帧）。
type probeEntry struct {
	Seq      int
	Phase    string
	Address  uint16
	Quantity uint16
	Start    time.Time
	Dur      time.Duration
	Result   string   // SUCCESS，或 aiErrorKind 的错误种类
	Err      string   // 错误说明
	Words    []uint16 // 正常响应的原始字（线圈为 0 / 1）
	Frames   []modbus.Packet
}

// Log 按顺序记下一次检测的全部请求，导出报告用。请求由检测 goroutine 写，帧由收发回调写，互斥保护。
type Log struct {
	mu      sync.Mutex
	entries []probeEntry
	current int // 正在等待响应的请求下标，-1 表示没有
}

func NewLog() *Log { return &Log{current: -1} }

func (l *Log) begin(phase string, req modbus.Request) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, probeEntry{Seq: len(l.entries) + 1, Phase: phase, Address: req.Address, Quantity: req.Quantity, Start: time.Now()})
	l.current = len(l.entries) - 1
	return l.current
}

func (l *Log) end(i int, resp *modbus.Response, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := &l.entries[i]
	e.Dur = time.Since(e.Start)
	e.Result = "SUCCESS"
	switch {
	case err == nil:
		e.Words = slices.Clone(resp.Registers)
		for _, b := range resp.Bits {
			e.Words = append(e.Words, map[bool]uint16{true: 1}[b])
		}
	case errors.Is(err, context.Canceled):
		e.Result, e.Err = "CANCELLED", err.Error()
	default:
		e.Result, e.Err = diagnose.Kind(err), err.Error()
	}
	l.current = -1
}

// frame 记下收发回调里的一帧，归到正在等待的请求上；检测之外的帧不记。
func (l *Log) Frame(p modbus.Packet) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current >= 0 {
		p.Raw = bytes.Clone(p.Raw)
		l.entries[l.current].Frames = append(l.entries[l.current].Frames, p)
	}
}

// snapshot 返回检测结束后的全部请求。
func (l *Log) Snapshot() []probeEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// Logged 把每条请求和结果记进 Log。
type Logged struct {
	Client
	Log *Log
}

func (c Logged) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
	phase, _ := ctx.Value(probePhaseKey{}).(string)
	i := c.Log.begin(phase, req)
	resp, err := c.Client.Do(ctx, req)
	c.Log.end(i, resp, err)
	return resp, err
}

// Run 是一次检测的条件和全部请求，检测结束后不再改变，导出报告从这里取数据。
type Run struct {
	Mode              Mode
	Def               Def
	Target            string // 连接入口：协议和地址，或串口参数
	Timeout           time.Duration
	Started, Finished time.Time
	StopErr           error
	Log               *Log
	Points            []point // 本次检测用到的点
}

// Output 是一次检测的结果：界面显示的文字、首行摘要，以及导出的失败清单。
type Output struct {
	Text, Summary string
	Warn          bool
	CSVHeader     []string
	CSVRows       [][]string
}

// pointsDigest 是本次用到的点的摘要：按地址、类型、宽度、字节序和名称算 SHA-256，用来核对报告对应哪一版点表。
func pointsDigest(ps []point) string {
	h := sha256.New()
	for _, p := range ps {
		fmt.Fprintf(h, "%d\t%d\t%s\t%d\t%s\t%s\n", p.Area, p.Offset, p.Type, p.Regs(), p.Order, p.Name)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Run) endState() string {
	switch {
	case errors.Is(r.StopErr, context.Canceled):
		return "已停止（使用者停止），只包含已完成的请求"
	case r.StopErr != nil:
		return "中途停止：" + r.StopErr.Error()
	}
	return "完成"
}

// phaseStats 按读取类别统计请求数和结果。没有执行的类别也列出，标明“本次未执行”。
func phaseStats(entries []probeEntry) string {
	type stat struct{ n, ok, nonstandard, other int }
	stats := map[string]*stat{}
	for _, e := range entries {
		s := stats[e.Phase]
		if s == nil {
			s = &stat{}
			stats[e.Phase] = s
		}
		s.n++
		switch e.Result {
		case "SUCCESS":
			s.ok++
		case "NONSTANDARD_REQUEST_ECHO", "TIMEOUT_AFTER_MISMATCHED_FRAME":
			s.nonstandard++
		default:
			s.other++
		}
	}
	var b strings.Builder
	b.WriteString("| 读取方式 | 请求数 | 正常响应 | 非标准响应 | 其他失败 |\n| --- | --- | --- | --- | --- |\n")
	for _, ph := range []string{PhaseBatch, PhasePoint, PhaseWord, PhaseCandidate, PhaseControl, PhaseBoundary} {
		s := stats[ph]
		if s == nil {
			if ph == PhaseBatch || ph == PhasePoint || ph == PhaseWord {
				fmt.Fprintf(&b, "| %s | 0 | 本次未执行 | | |\n", phaseNames[ph])
			}
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n", phaseNames[ph], s.n, s.ok, s.nonstandard, s.other)
	}
	return b.String()
}

// reportMarkdown 是报告正文：检测条件、各读取方式统计、结果原文、明细文件链接和完整性说明。
func reportMarkdown(r *Run, out Output, entries []probeEntry, note, base string, csvRows int) string {
	d := r.Def
	var b strings.Builder
	fmt.Fprintf(&b, "# Modbus 检测报告\n\n")
	fmt.Fprintf(&b, "| 项目 | 内容 |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| 检测方式 | %s |\n", ModeNames[r.Mode])
	fmt.Fprintf(&b, "| 开始 – 结束 | %s – %s（用时 %s） |\n", r.Started.Format("2006-01-02 15:04:05"), r.Finished.Format("15:04:05"), r.Finished.Sub(r.Started).Round(time.Second))
	fmt.Fprintf(&b, "| 结束情况 | %s |\n", r.endState())
	fmt.Fprintf(&b, "| 连接入口 | %s |\n", r.Target)
	if note == "" {
		note = "实际设备地址未提供"
	}
	fmt.Fprintf(&b, "| 实际设备（使用者备注） | %s |\n", note)
	fmt.Fprintf(&b, "| Slave ID / 功能码 | %d / %s |\n", d.Slave, d.Function)
	fmt.Fprintf(&b, "| 检测范围 | %s（Offset %d–%d） |\n", refSpan(d.area(), d.Start, d.Qty), d.Start, int(d.Start)+d.Qty-1)
	fmt.Fprintf(&b, "| 响应超时 | %d ms |\n", r.Timeout.Milliseconds())
	if len(r.Points) > 0 {
		fmt.Fprintf(&b, "| 点表 | 本次用到 %d 个点，SHA-256 `%s` |\n", len(r.Points), pointsDigest(r.Points))
	}
	fmt.Fprintf(&b, "\n## 各读取方式的请求\n\n%s\n", phaseStats(entries))
	fmt.Fprintf(&b, "## 结果\n\n**%s**\n\n```text\n%s\n```\n\n", out.Summary, out.Text)
	fmt.Fprintf(&b, "## 明细文件\n\n- [请求与原始报文（JSONL，%d 条请求）](<%s.jsonl>)\n", len(entries), base)
	fmt.Fprintf(&b, "- [读不通的点 / 地址清单（CSV，%d 行）](<%s-失败清单.csv>)\n\n", csvRows, base)
	fmt.Fprintf(&b, "## 完整性\n\n")
	fmt.Fprintf(&b, "- 本次检测共发出 %d 条请求，JSONL 逐条记录请求参数、结果、正常响应的原始字，以及等待这条请求期间收到的全部原始帧（含晚到和不匹配的帧）。\n", len(entries))
	fmt.Fprintf(&b, "- 结束情况：%s。未检测的点或地址不算读通。\n", r.endState())
	fmt.Fprintf(&b, "- 同样的收发也在本机报文数据库里（packets 表；异常的另存 anomalies 表）。\n")
	fmt.Fprintf(&b, "- 报告只记录读取行为和报文事实；批量返回的 0 是否对应真实值、设备内部是哪项配置原因，需要现场核对。\n")
	return b.String()
}

// JSONLEntry 是 JSONL 里的一行。
type JSONLEntry struct {
	Seq        int          `json:"seq"`
	Phase      string       `json:"phase"`
	PhaseName  string       `json:"phase_name"`
	Slave      byte         `json:"slave"`
	Function   byte         `json:"function"`
	Address    uint16       `json:"address"`
	Quantity   uint16       `json:"quantity"`
	Reference  string       `json:"reference"`
	Started    string       `json:"started"`
	DurationMS float64      `json:"duration_ms"`
	Result     string       `json:"result"`
	Error      string       `json:"error,omitempty"`
	Words      []uint16     `json:"words,omitempty"`
	Frames     []JSONLFrame `json:"frames"`
}

type JSONLFrame struct {
	Time         string  `json:"time"`
	Direction    string  `json:"direction"`
	Status       string  `json:"status"`
	ConnectionID string  `json:"connection_id,omitempty"`
	RequestID    uint64  `json:"request_id"`
	TxID         uint16  `json:"tx_id,omitempty"`
	RTTMS        float64 `json:"rtt_ms,omitempty"`
	Hex          string  `json:"hex"`
	Error        string  `json:"error,omitempty"`
}

func writeJSONL(path string, d Def, entries []probeEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		line := JSONLEntry{Seq: e.Seq, Phase: e.Phase, PhaseName: phaseNames[e.Phase], Slave: d.Slave, Function: byte(d.Function),
			Address: e.Address, Quantity: e.Quantity, Reference: refSpan(d.area(), e.Address, int(e.Quantity)),
			Started: e.Start.Format(time.RFC3339Nano), DurationMS: float64(e.Dur) / float64(time.Millisecond),
			Result: e.Result, Error: e.Err, Words: e.Words, Frames: []JSONLFrame{}}
		for _, p := range e.Frames {
			fr := JSONLFrame{Time: p.Time.Format(time.RFC3339Nano), Direction: string(p.Dir), Status: string(p.Status), ConnectionID: p.ConnectionID,
				RequestID: p.RequestID, TxID: p.TxID, RTTMS: float64(p.RTT) / float64(time.Millisecond), Hex: hexs(p.Raw)}
			if p.Err != nil {
				fr.Error = p.Err.Error()
			}
			line.Frames = append(line.Frames, fr)
		}
		if err = enc.Encode(line); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// writeCSV 写失败清单，带 UTF-8 BOM，Excel 直接打开中文不乱码。
func writeCSV(path string, header []string, rows [][]string) error {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	w.Write(header)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// verifyReport 读回导出的文件，核对 JSONL 行数、CSV 行数与报告一致，每行 JSON 都能解析。
func verifyReport(mdPath, jsonlPath, csvPath string, md string, entries, rows int) error {
	b, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	if string(b) != md {
		return fmt.Errorf("报告正文读回与写入不一致")
	}
	f, err := os.Open(jsonlPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	n := 0
	for sc.Scan() {
		var v JSONLEntry
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return fmt.Errorf("JSONL 第 %d 行读不出来：%w", n+1, err)
		}
		n++
		if v.Seq != n {
			return fmt.Errorf("JSONL 第 %d 行序号是 %d", n, v.Seq)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n != entries {
		return fmt.Errorf("JSONL 有 %d 行，检测发出 %d 条请求", n, entries)
	}
	c, err := os.ReadFile(csvPath)
	if err != nil {
		return err
	}
	records, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(c, []byte("\xEF\xBB\xBF")))).ReadAll()
	if err != nil {
		return fmt.Errorf("CSV 读不出来：%w", err)
	}
	if len(records) != rows+1 {
		return fmt.Errorf("CSV 有 %d 行数据，应为 %d 行", len(records)-1, rows)
	}
	return nil
}

// ExportReport 把报告正文写到 mdPath，同目录写同名的 JSONL 明细和 CSV 失败清单，再读回核对。
// 返回写出的文件；出错时删掉已写的文件，不留下不完整的报告。
func ExportReport(ctx context.Context, r *Run, out Output, note, mdPath string) ([]string, error) {
	entries := r.Log.Snapshot()
	base := strings.TrimSuffix(filepath.Base(mdPath), filepath.Ext(mdPath))
	dir := filepath.Dir(mdPath)
	jsonlPath := filepath.Join(dir, base+".jsonl")
	csvPath := filepath.Join(dir, base+"-失败清单.csv")
	files := []string{mdPath, jsonlPath, csvPath}
	md := reportMarkdown(r, out, entries, note, base, len(out.CSVRows))
	err := writeJSONL(jsonlPath, r.Def, entries)
	if err == nil && ctx.Err() == nil {
		err = writeCSV(csvPath, out.CSVHeader, out.CSVRows)
	}
	if err == nil && ctx.Err() == nil {
		err = os.WriteFile(mdPath, []byte(md), 0o644)
	}
	if err == nil && ctx.Err() == nil {
		err = verifyReport(mdPath, jsonlPath, csvPath, md, len(entries), len(out.CSVRows))
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		for _, f := range files {
			_ = os.Remove(f)
		}
		return nil, err
	}
	return files, nil
}
