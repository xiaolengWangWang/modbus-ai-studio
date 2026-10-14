package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/diagnose"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
	"modbus-ai-studio/internal/recorder"
)

// TableConfig 是一个读取表：按间隔读一段地址，逐行显示。
type TableConfig struct {
	Name       string        `json:"name"`
	Slave      int           `json:"slave"`
	Function   int           `json:"function"` // 1 线圈、2 离散输入、3 保持寄存器、4 输入寄存器
	Address    string        `json:"address"`  // 起始地址：0、40001、4x0001、0x0010……
	Count      int           `json:"count"`
	Type       string        `json:"type"`  // 寄存器的数据类型，默认 UINT16
	Order      string        `json:"order"` // 字节序，默认标准大端；64 位写 32 位的名字时按同样规则换算
	Scale      float64       `json:"scale"` // 工程值 = 原始值 × Scale，默认 1
	IntervalMs int           `json:"intervalMs"`
	Points     []PointConfig `json:"points,omitempty"` // 点表：有时每行是一个点，类型各自不同
}

// PointConfig 是点表里的一个点，和桌面版的点表一样：类型可以是 BOOL、STRING 和各种数值类型（含 REAL、DINT 等别名）。
type PointConfig struct {
	Offset   uint16         `json:"offset"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Order    string         `json:"order"`
	Scale    float64        `json:"scale"`
	Unit     string         `json:"unit"`
	Writable bool           `json:"writable"`
	Min      *float64       `json:"min,omitempty"`
	Max      *float64       `json:"max,omitempty"`
	Enum     map[int]string `json:"enum,omitempty"`   // 状态类点的取值含义，例如 0=停止
	Len      int            `json:"length,omitempty"` // STRING 的字符数
}

// point 把配置换成点表的点，补全默认值并检查。
func (pc PointConfig) point(area modbus.Area) (points.Point, error) {
	t, ok := points.ParseType(pc.Type)
	if !ok {
		return points.Point{}, fmt.Errorf("不支持的数据类型 %q", pc.Type)
	}
	p := points.Point{Area: area, Offset: pc.Offset, Name: pc.Name, Type: t, Order: modbus.ByteOrder(pc.Order), Scale: pc.Scale,
		Unit: pc.Unit, RW: pc.Writable, Min: pc.Min, Max: pc.Max, Enum: pc.Enum, Len: pc.Len}
	return p, p.Normalize()
}

// configOf 是点在接口里的写法。
func configOf(p points.Point) PointConfig {
	return PointConfig{Offset: p.Offset, Name: p.Name, Type: string(p.Type), Order: string(p.Order), Scale: p.Scale, Unit: p.Unit,
		Writable: p.RW, Min: p.Min, Max: p.Max, Enum: p.Enum, Len: p.Len}
}

// RowView 是读取表的一行。Type / Order / Scale 供页面预填写入。
type RowView struct {
	Ref      string  `json:"ref"`
	Offset   int     `json:"offset"`
	Name     string  `json:"name,omitempty"`
	Regs     string  `json:"regs"`
	Value    string  `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Hint     string  `json:"hint,omitempty"`
	Type     string  `json:"type"`
	Order    string  `json:"order"`
	Scale    float64 `json:"scale"`
	Writable bool    `json:"writable"`
}

type table struct {
	id     int
	cfg    TableConfig
	fn     modbus.FunctionCode
	start  uint16
	points []points.Point // 寄存器表没有点表时按 Count 和类型均匀分行；线圈、离散输入表没有点表时逐个显示

	mu       sync.Mutex
	paused   bool
	cancel   context.CancelFunc
	status   string // waiting、ok、error、paused、offline
	err      string
	hint     string // 出错时的可能原因和处理建议（internal/diagnose，和桌面版同一套规则）
	rtt      float64
	updated  time.Time
	polls    int
	fails    int
	lastFail string // 同一种错误连续出现只记一次日志
	rows     []RowView
}

var dataTypes = []modbus.DataType{modbus.TypeUint16, modbus.TypeInt16, modbus.TypeUint32, modbus.TypeInt32,
	modbus.TypeFloat32, modbus.TypeUint64, modbus.TypeInt64, modbus.TypeFloat64}

// parseType 解析数据类型和字节序；字节序为空时用标准大端。
func parseType(typ, order string) (modbus.DataType, modbus.ByteOrder, error) {
	dt := modbus.DataType(strings.ToUpper(strings.TrimSpace(typ)))
	if dt == "" {
		dt = modbus.TypeUint16
	}
	if !slices.Contains(dataTypes, dt) {
		return "", "", fmt.Errorf("不支持的数据类型 %q", typ)
	}
	o := modbus.ByteOrder(strings.ToUpper(strings.TrimSpace(order)))
	if o == "" {
		o = modbus.OrderAB
	}
	o = o.For(dt)
	if !slices.Contains(dt.Orders(), o) {
		return "", "", fmt.Errorf("%s 不能用字节序 %s", dt, order)
	}
	return dt, o, nil
}

// resolveAddress 把用户输入的地址解析为 Offset。40001 这类写法必须与数据区一致，不静默猜测。
func resolveAddress(s string, area modbus.Area) (uint16, error) {
	cands, err := modbus.ParseAddress(s)
	if err != nil {
		return 0, err
	}
	for _, c := range cands {
		if c.Area == area {
			return c.Offset, nil
		}
	}
	if cands[0].Area == modbus.AreaNone {
		return cands[0].Offset, nil
	}
	msg := fmt.Sprintf("地址 %s 是 %s 区，与所选功能码的 %s 区不符", s, cands[0].Area.Prefix(), area.Prefix())
	if raw := cands[len(cands)-1]; raw.Area == modbus.AreaNone {
		msg += fmt.Sprintf("；如果指原始 Offset %d，请写成 0x%04X", raw.Offset, raw.Offset)
	}
	return 0, errors.New(msg)
}

func newTable(cfg TableConfig) (*table, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	fn := modbus.FunctionCode(cfg.Function)
	if !fn.IsRead() {
		return nil, errors.New("功能码应为 01、02、03 或 04")
	}
	if cfg.Slave < 0 || cfg.Slave > 255 {
		return nil, errors.New("Slave ID 应为 0–255")
	}
	start, err := resolveAddress(cfg.Address, modbus.AreaOf(fn))
	if err != nil {
		return nil, err
	}
	if cfg.IntervalMs == 0 {
		cfg.IntervalMs = 1000
	}
	if cfg.IntervalMs < 100 || cfg.IntervalMs > 3_600_000 {
		return nil, errors.New("读取间隔应在 100 ms 到 1 小时之间")
	}
	if cfg.Scale == 0 {
		cfg.Scale = 1
	}
	t := &table{cfg: cfg, fn: fn, start: start, status: "waiting"}
	if cfg.Count < 1 || cfg.Count > 2000 || (modbus.Request{Slave: byte(cfg.Slave), Function: fn, Address: start, Quantity: uint16(cfg.Count)}).Validate() != nil {
		return nil, fmt.Errorf("数量不对：线圈 / 离散输入一次最多 2000 个，寄存器最多 125 个，且不能超出 65535")
	}
	area := modbus.AreaOf(fn)
	bits := fn == modbus.FuncReadCoils || fn == modbus.FuncReadDiscreteInputs
	if bits {
		cfg.Type, cfg.Order = "", ""
	} else if len(cfg.Points) == 0 {
		dt, order, err := parseType(cfg.Type, cfg.Order)
		if err != nil {
			return nil, err
		}
		cfg.Type, cfg.Order = string(dt), string(order)
		w := dt.Registers()
		if cfg.Count%w != 0 {
			return nil, fmt.Errorf("%s 每个值占 %d 个寄存器，数量应为 %d 的倍数", dt, w, w)
		}
		for off := 0; off < cfg.Count; off += w {
			t.points = append(t.points, points.Point{Area: area, Offset: start + uint16(off), Type: dt, Order: order, Scale: cfg.Scale,
				RW: fn == modbus.FuncReadHoldingRegisters})
		}
	}
	t.cfg = cfg
	if len(cfg.Points) == 0 {
		return t, nil
	}
	t.cfg.Points = nil
	for _, pc := range cfg.Points {
		p, err := pc.point(area)
		if err != nil {
			return nil, fmt.Errorf("点 %s：%w", pc.Name, err)
		}
		if p.Offset < start || int(p.Offset)+p.Regs() > int(start)+cfg.Count {
			return nil, fmt.Errorf("点 %s 不在读取范围内", p.Name)
		}
		p.RW = p.RW && (fn == modbus.FuncReadHoldingRegisters || fn == modbus.FuncReadCoils) && p.Type != points.TypeString
		t.points = append(t.points, p)
		t.cfg.Points = append(t.cfg.Points, configOf(p))
	}
	return t, nil
}

// title 是表头的一行说明，例如 Slave 1 · FC03 · 40001 × 10 · FLOAT32 CDAB。
func (t *table) title() string {
	area := modbus.AreaOf(t.fn)
	s := fmt.Sprintf("Slave %d · FC%02d · %s × %d", t.cfg.Slave, t.fn, modbus.Reference(area, t.start), t.cfg.Count)
	if len(t.cfg.Points) > 0 {
		return s + fmt.Sprintf(" · 点表 %d 个点", len(t.points))
	}
	if t.cfg.Type != "" {
		s += " · " + t.cfg.Type + " " + t.cfg.Order
	}
	if t.cfg.Scale != 1 {
		s += " · ×" + strconv.FormatFloat(t.cfg.Scale, 'g', -1, 64)
	}
	return s
}

func (t *table) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
}

func (s *Server) startTable(t *table) {
	ctx, cancel := context.WithCancel(s.ctx)
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
	s.wg.Go(func() {
		for {
			s.poll(ctx, t)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(t.cfg.IntervalMs) * time.Millisecond):
			}
		}
	})
}

// poll 读一次。连接错误交给 device 重连；同一种错误连续出现只记一次日志，恢复时再记一次。
func (s *Server) poll(ctx context.Context, t *table) {
	t.mu.Lock()
	paused := t.paused
	t.mu.Unlock()
	c := s.dev.current()
	probing, scanning := s.probing(), s.scanning()
	if paused || probing || scanning || c == nil {
		status := "offline" // 连接的原因在顶部状态和日志里
		switch {
		case paused:
			status = "paused"
		case probing:
			status = "probing"
		case scanning:
			status = "scanning"
		}
		t.mu.Lock()
		t.status, t.err = status, ""
		t.mu.Unlock()
		s.changed()
		return
	}
	req := modbus.Request{Slave: byte(t.cfg.Slave), Function: t.fn, Address: t.start, Quantity: uint16(t.cfg.Count)}
	begin := time.Now()
	s.pollGate.RLock() // 搜索站号时在这里等
	resp, err := c.Do(ctx, req)
	s.pollGate.RUnlock()
	if ctx.Err() != nil || errors.Is(err, modbus.ErrConnection) && s.probing() {
		return // 检测开始时让出了连接，在途的读取断开，不算读取失败
	}
	if errors.Is(err, modbus.ErrConnection) {
		s.dev.linkDown(c, err)
	}
	// 出错分析在锁表之前做：要读设备的连接参数（设备的锁）
	errText, hint := "", ""
	if err != nil {
		errText = explain(err)
		if !errors.Is(err, modbus.ErrConnection) { // 断开交给自动重连，顶部状态和日志里说原因
			r := diagnose.Analyze(diagnose.Request{Slave: byte(t.cfg.Slave), Function: t.fn, Start: t.start, Qty: t.cfg.Count,
				Scan: time.Duration(t.cfg.IntervalMs) * time.Millisecond}, s.dev.diagContext(), err)
			errText, hint = r.Text, strings.TrimSpace(r.Hint+" "+webAdvice(r))
		}
	}
	t.mu.Lock()
	t.polls++
	t.updated = time.Now()
	var logKind, logDetail string
	if err != nil {
		t.fails++
		t.status, t.err, t.hint = "error", errText, hint
		if t.err != t.lastFail {
			t.lastFail = t.err
			logKind, logDetail = recorder.EventReadFail, fmt.Sprintf("%s 读取失败：%s", t.label(), t.err)
		}
	} else {
		t.status, t.err, t.hint, t.rtt = "ok", "", "", float64(time.Since(begin).Microseconds())/1000
		t.rows = t.decode(resp)
		if t.lastFail != "" {
			t.lastFail = ""
			logKind, logDetail = recorder.EventReadOK, t.label()+" 恢复正常"
		}
	}
	t.mu.Unlock()
	if logKind != "" {
		s.dev.mu.Lock()
		session := s.dev.session
		s.dev.mu.Unlock()
		s.dev.log(session, logKind, logDetail, t.id)
	}
	s.changed()
}

func (t *table) label() string {
	if t.cfg.Name != "" {
		return fmt.Sprintf("读取表 %d「%s」", t.id, t.cfg.Name)
	}
	return fmt.Sprintf("读取表 %d", t.id)
}

// webAdvice 把一键处理换成网页上的说法：网页上大多没有对应的按钮，告诉用户怎么做。
func webAdvice(r diagnose.Result) string {
	switch r.Action {
	case diagnose.SetTimeout:
		return fmt.Sprintf("建议把连接设置里的超时改为 %d ms 后重新连接。", r.Timeout.Milliseconds())
	case diagnose.UseFC03, diagnose.UseFC04:
		return "建议编辑读取表，" + r.Label + "。"
	case diagnose.HalveQty:
		return "建议编辑读取表，把数量减半。"
	case diagnose.DoubleScan:
		return "建议编辑读取表，把读取间隔加倍。"
	case diagnose.ProbeRange:
		return "建议缩小读取范围逐段核对；桌面版的“检测寄存器”可以自动逐点核对。"
	case diagnose.ScanSlaves:
		return "点“搜索站号”可以找出在应答的站号。"
	case diagnose.DetectProtocol:
		return "建议点“识别协议”。"
	case diagnose.ScanSerial:
		return "桌面版的“扫描串口参数”可以逐个试波特率和校验位。"
	}
	return ""
}

// explain 把错误说成现场能直接处理的话（写入、连接这类不按读取表分析的错误）。
func explain(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("%v。建议：%s", ex, ex.Code.Tip())
	}
	if ns, ok := modbus.AsNonstandard(err); ok {
		return fmt.Sprintf("非标准响应：功能码 %02X 原样回显了请求的地址和数量，没有寄存器数据，后面的字节不是异常码。设备收到了请求，"+
			"但这个范围可能没有转发数据：缩小范围逐个核对哪些地址读不通，再到设备的转发配置里核对", ns.PDU[0])
	}
	if errors.Is(err, modbus.ErrTimeout) && errors.Is(err, modbus.ErrMismatch) {
		return err.Error() + "：设备在应答，只是内容对不上，先看报文里标为不匹配的帧"
	}
	if errors.Is(err, modbus.ErrTimeout) {
		return err.Error() + "：检查 Slave ID、协议类型和接线"
	}
	if errors.Is(err, modbus.ErrConnection) {
		return "连接断开，正在自动重连：原因见顶部的连接状态和日志"
	}
	return err.Error()
}

// decode 把一次读到的数据排成行。值的格式和桌面版一样（points.Point.Text）：带枚举含义，
// 64 位整数不丢精度，STRING 解成文字，浮点数不合理时提示更可能的字节序。
func (t *table) decode(resp *modbus.Response) []RowView {
	area := modbus.AreaOf(t.fn)
	bit := func(i int) string { return map[bool]string{true: "1", false: "0"}[resp.Bits[i]] }
	if len(t.points) == 0 { // 线圈、离散输入
		rows := make([]RowView, 0, len(resp.Bits))
		for i := range resp.Bits {
			off := t.start + uint16(i)
			rows = append(rows, RowView{Ref: modbus.Reference(area, off), Offset: int(off), Regs: bit(i), Value: bit(i), Writable: t.fn == modbus.FuncReadCoils})
		}
		return rows
	}
	rows := make([]RowView, 0, len(t.points))
	for _, p := range t.points {
		i := int(p.Offset - t.start)
		row := RowView{Ref: modbus.Reference(area, p.Offset), Offset: int(p.Offset), Name: p.Name, Unit: p.Unit,
			Type: string(p.Type), Order: string(p.Order), Scale: p.Scale, Writable: p.RW}
		var regs []uint16
		if t.fn == modbus.FuncReadCoils || t.fn == modbus.FuncReadDiscreteInputs {
			row.Regs, row.Type, row.Order = bit(i), "", ""
			regs = []uint16{0}
			if resp.Bits[i] {
				regs[0] = 1
			}
		} else {
			regs = resp.Registers[i : i+p.Regs()]
			hexRegs := make([]string, len(regs))
			for j, r := range regs {
				hexRegs[j] = fmt.Sprintf("%04X", r)
			}
			row.Regs = strings.Join(hexRegs, " ")
		}
		value, plausible, err := p.Text(regs)
		switch {
		case err != nil:
			value, row.Hint = "—", err.Error()
		case !plausible:
			row.Hint = "数值不合理，字节序可能不对"
			if sug, ok := modbus.SuggestByteOrder(p.Order, regs); ok && p.Type == modbus.TypeFloat32 {
				row.Hint = "数值不合理，按 " + string(sug) + " 解码更可能正确"
			}
		}
		row.Value = value
		rows = append(rows, row)
	}
	return rows
}

type tableView struct {
	ID       int         `json:"id"`
	Config   TableConfig `json:"config"`
	Title    string      `json:"title"`
	Writable bool        `json:"writable"`
	Paused   bool        `json:"paused"`
	Status   string      `json:"status"`
	Error    string      `json:"error,omitempty"`
	Hint     string      `json:"hint,omitempty"`
	RTT      float64     `json:"rtt"`
	Updated  string      `json:"updated"`
	Polls    int         `json:"polls"`
	Fails    int         `json:"fails"`
	Rows     []RowView   `json:"rows"`
}

func (t *table) view() tableView {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := tableView{ID: t.id, Config: t.cfg, Title: t.title(), Writable: t.fn == modbus.FuncReadCoils || t.fn == modbus.FuncReadHoldingRegisters,
		Paused: t.paused, Status: t.status, Error: t.err, Hint: t.hint, RTT: t.rtt, Polls: t.polls, Fails: t.fails, Rows: t.rows}
	if !t.updated.IsZero() {
		v.Updated = t.updated.Format("15:04:05")
	}
	if v.Rows == nil {
		v.Rows = []RowView{}
	}
	return v
}

type recordView struct {
	Enabled bool   `json:"enabled"`
	File    string `json:"file"`
	Dropped int64  `json:"dropped"`
}

type stateView struct {
	Version string      `json:"version"`
	Conn    connView    `json:"conn"`
	Tables  []tableView `json:"tables"`
	Logs    []logView   `json:"logs"`
	Record  recordView  `json:"record"`
	Probe   *probeBrief `json:"probe,omitempty"` // 最近一次检测寄存器的进度或结论
	Scan    *scanView   `json:"scan,omitempty"`  // 最近一次搜索站号的进度或结果
}

func (s *Server) state() stateView {
	v := stateView{Version: s.opts.Version, Conn: s.dev.view(), Logs: s.logs.all(), Tables: []tableView{}}
	s.mu.Lock()
	tables := slices.Clone(s.tables)
	s.mu.Unlock()
	for _, t := range tables {
		v.Tables = append(v.Tables, t.view())
	}
	if r := s.opts.Recorder; r != nil {
		v.Record = recordView{Enabled: true, File: baseName(r.Path()), Dropped: r.Dropped.Load()}
	}
	s.mu.Lock()
	j, sj := s.probe, s.scan
	s.mu.Unlock()
	if j != nil {
		b := j.brief()
		v.Probe = &b
	}
	if sj != nil {
		sv := sj.snapshot()
		v.Scan = &sv
	}
	return v
}
