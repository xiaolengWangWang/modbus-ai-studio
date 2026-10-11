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

	"modbus-ai-studio/internal/modbus"
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

// PointConfig 是点表里的一个点。
type PointConfig struct {
	Offset   uint16  `json:"offset"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Order    string  `json:"order"`
	Scale    float64 `json:"scale"`
	Unit     string  `json:"unit"`
	Writable bool    `json:"writable"`
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

type point struct {
	PointConfig
	dt    modbus.DataType
	order modbus.ByteOrder
}

type table struct {
	id     int
	cfg    TableConfig
	fn     modbus.FunctionCode
	start  uint16
	points []point // 没有点表时按 Count 和类型均匀分行

	mu       sync.Mutex
	paused   bool
	cancel   context.CancelFunc
	status   string // waiting、ok、error、paused、offline
	err      string
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
	bits := fn == modbus.FuncReadCoils || fn == modbus.FuncReadDiscreteInputs
	if bits {
		cfg.Type, cfg.Order, cfg.Points = "", "", nil
		t.cfg = cfg
		return t, nil
	}
	dt, order, err := parseType(cfg.Type, cfg.Order)
	if err != nil {
		return nil, err
	}
	t.cfg.Type, t.cfg.Order = string(dt), string(order)
	if len(cfg.Points) == 0 {
		w := dt.Registers()
		if cfg.Count%w != 0 {
			return nil, fmt.Errorf("%s 每个值占 %d 个寄存器，数量应为 %d 的倍数", dt, w, w)
		}
		for off := 0; off < cfg.Count; off += w {
			t.points = append(t.points, point{PointConfig: PointConfig{Offset: start + uint16(off), Scale: cfg.Scale, Writable: fn == modbus.FuncReadHoldingRegisters}, dt: dt, order: order})
		}
		return t, nil
	}
	for _, p := range cfg.Points {
		pdt, po, err := parseType(p.Type, p.Order)
		if err != nil {
			return nil, fmt.Errorf("点 %s：%w", p.Name, err)
		}
		if p.Offset < start || int(p.Offset)+pdt.Registers() > int(start)+cfg.Count {
			return nil, fmt.Errorf("点 %s 不在读取范围内", p.Name)
		}
		if p.Scale == 0 {
			p.Scale = 1
		}
		p.Writable = p.Writable && fn == modbus.FuncReadHoldingRegisters
		t.points = append(t.points, point{PointConfig: p, dt: pdt, order: po})
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
	if paused || c == nil {
		t.mu.Lock()
		t.status, t.err = map[bool]string{true: "paused", false: "offline"}[paused], "" // 连接的原因在顶部状态和日志里
		t.mu.Unlock()
		s.changed()
		return
	}
	req := modbus.Request{Slave: byte(t.cfg.Slave), Function: t.fn, Address: t.start, Quantity: uint16(t.cfg.Count)}
	begin := time.Now()
	resp, err := c.Do(ctx, req)
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, modbus.ErrConnection) {
		s.dev.linkDown(c, err)
	}
	t.mu.Lock()
	t.polls++
	t.updated = time.Now()
	var logKind, logDetail string
	if err != nil {
		t.fails++
		t.status, t.err = "error", explain(err)
		if t.err != t.lastFail {
			t.lastFail = t.err
			logKind, logDetail = recorder.EventReadFail, fmt.Sprintf("%s 读取失败：%s", t.label(), t.err)
		}
	} else {
		t.status, t.err, t.rtt = "ok", "", float64(time.Since(begin).Microseconds())/1000
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

// explain 把错误说成现场能直接处理的话。
func explain(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("%v。建议：%s", ex, ex.Code.Tip())
	}
	if errors.Is(err, modbus.ErrTimeout) {
		return err.Error() + "：检查 Slave ID、协议类型和接线"
	}
	if errors.Is(err, modbus.ErrConnection) {
		return "连接断开，正在自动重连：原因见顶部的连接状态和日志"
	}
	return err.Error()
}

func (t *table) decode(resp *modbus.Response) []RowView {
	area := modbus.AreaOf(t.fn)
	if len(t.points) == 0 { // 线圈、离散输入
		rows := make([]RowView, 0, len(resp.Bits))
		for i, b := range resp.Bits {
			off := t.start + uint16(i)
			v := map[bool]string{true: "1", false: "0"}[b]
			rows = append(rows, RowView{Ref: modbus.Reference(area, off), Offset: int(off), Regs: v, Value: v, Writable: t.fn == modbus.FuncReadCoils})
		}
		return rows
	}
	rows := make([]RowView, 0, len(t.points))
	for _, p := range t.points {
		i := int(p.Offset - t.start)
		regs := resp.Registers[i : i+p.dt.Registers()]
		hexRegs := make([]string, len(regs))
		for j, r := range regs {
			hexRegs[j] = fmt.Sprintf("%04X", r)
		}
		value, hint := formatValue(p.dt, p.order, p.Scale, regs)
		rows = append(rows, RowView{Ref: modbus.Reference(area, p.Offset), Offset: int(p.Offset), Name: p.Name, Regs: strings.Join(hexRegs, " "),
			Value: value, Unit: p.Unit, Hint: hint, Type: string(p.dt), Order: string(p.order), Scale: p.Scale, Writable: p.Writable})
	}
	return rows
}

// formatValue 显示工程值：整型不换算时精确显示（64 位不经过 float64），FLOAT32 按单精度显示，
// 有倍率时按倍率的小数位数显示。FLOAT32 明显不合理时提示更可能的字节序。
func formatValue(dt modbus.DataType, o modbus.ByteOrder, scale float64, regs []uint16) (value, hint string) {
	sc := modbus.Scaling{Scale: scale}
	if sc.Identity() && !dt.Float() {
		if s, err := modbus.FormatInt(dt, o, regs); err == nil {
			return s, ""
		}
	}
	raw, err := modbus.DecodeRaw(dt, o, regs)
	if err != nil {
		return "—", err.Error()
	}
	if dt == modbus.TypeFloat32 {
		if sug, ok := modbus.SuggestByteOrder(o, regs); ok {
			hint = "数值不合理，按 " + string(sug) + " 解码更可能正确"
		}
	}
	v := sc.Engineering(raw)
	switch {
	case dt == modbus.TypeFloat32 && sc.Identity():
		return strconv.FormatFloat(v, 'g', -1, 32), hint
	case dt == modbus.TypeFloat64 && sc.Identity():
		return strconv.FormatFloat(v, 'g', -1, 64), hint
	}
	s := strconv.FormatFloat(scale, 'f', -1, 64)
	decimals := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimals = len(s) - i - 1
	}
	if dt.Float() {
		decimals += 3
	}
	return strconv.FormatFloat(v, 'f', decimals, 64), hint
}

type tableView struct {
	ID       int         `json:"id"`
	Config   TableConfig `json:"config"`
	Title    string      `json:"title"`
	Writable bool        `json:"writable"`
	Paused   bool        `json:"paused"`
	Status   string      `json:"status"`
	Error    string      `json:"error,omitempty"`
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
		Paused: t.paused, Status: t.status, Error: t.err, RTT: t.rtt, Polls: t.polls, Fails: t.fails, Rows: t.rows}
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
	return v
}
