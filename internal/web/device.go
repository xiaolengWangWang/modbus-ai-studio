package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/internal/transport"
)

// ConnConfig 是连接参数。
type ConnConfig struct {
	Mode      string `json:"mode"`      // tcp、rtu-over-tcp、ascii-over-tcp、rtu、ascii
	Target    string `json:"target"`    // TCP 类：IP:端口
	Port      string `json:"port"`      // 串口：/dev/ttyUSB0 等
	Baud      int    `json:"baud"`      // 串口波特率，默认 9600
	DataBits  int    `json:"dataBits"`  // 默认 8，Modbus ASCII 常用 7
	Parity    string `json:"parity"`    // N、E、O
	StopBits  int    `json:"stopBits"`  // 1 或 2
	TimeoutMs int    `json:"timeoutMs"` // 响应超时，默认 1000
	Simulator bool   `json:"simulator"` // 连接内置换热站模拟器，忽略 Target
}

var modes = map[string]modbus.Mode{
	"tcp": modbus.ModeTCP, "rtu-over-tcp": modbus.ModeRTUOverTCP, "ascii-over-tcp": modbus.ModeASCIIOverTCP,
	"rtu": modbus.ModeRTU, "ascii": modbus.ModeASCII,
}

// normalize 检查参数并补上默认值。
func (c *ConnConfig) normalize() (modbus.Mode, error) {
	m, ok := modes[c.Mode]
	if !ok {
		return "", fmt.Errorf("不支持的协议 %q", c.Mode)
	}
	if c.TimeoutMs == 0 {
		c.TimeoutMs = 1000
	}
	if c.TimeoutMs < 50 || c.TimeoutMs > 60000 {
		return "", errors.New("响应超时应在 50–60000 ms")
	}
	switch {
	case m.Serial() && c.Simulator:
		return "", errors.New("内置模拟器只支持 TCP 类协议")
	case m.Serial():
		if c.Port == "" {
			return "", errors.New("请选择串口")
		}
		c.Baud, c.DataBits, c.StopBits = orDefault(c.Baud, 9600), orDefault(c.DataBits, 8), orDefault(c.StopBits, 1)
		if c.Parity == "" {
			c.Parity = "N"
		}
	case c.Simulator:
		c.Target = ""
	default:
		if _, _, err := net.SplitHostPort(c.Target); err != nil {
			return "", fmt.Errorf("目标地址应为 IP:端口，例如 192.168.1.10:502")
		}
	}
	return m, nil
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// describe 是给人看的连接说明，也作为记录里的 target。
func (c ConnConfig) describe(target string) string {
	if m := modes[c.Mode]; m.Serial() {
		return fmt.Sprintf("%s %d %d%s%d", c.Port, c.Baud, c.DataBits, c.Parity, c.StopBits)
	}
	if c.Simulator {
		return "内置模拟器 " + target
	}
	return target
}

const (
	stateDisconnected = "disconnected"
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
)

// reconnectEvery 是断线后重连的间隔，测试时调小。
var reconnectEvery = 3 * time.Second

// device 是唯一的设备连接，所有读取表和写入共用。客户端内部保证同一时刻只有一个未完成请求。
// 连接中途断开时每 3 秒重连一次，直到成功或用户断开。
type device struct {
	s *Server

	mu            sync.Mutex
	cfg           ConnConfig
	mode          modbus.Mode
	target        string // 实际连接的地址（内置模拟器时是本机端口）
	client        *modbus.Client
	state         string
	err           string
	since         time.Time
	local, remote string
	session       int64
	ctx           context.Context // 本次连接，断开时结束（含重连和模拟器）
	cancel        context.CancelFunc
	netErr        error // 最近一次连接错误的原始错误（客户端返回的错误只留了文字），用来判断断开方式
}

// current 返回可用的客户端，未连接时为 nil。
func (d *device) current() *modbus.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client
}

func (d *device) rec() *recorder.Recorder { return d.s.opts.Recorder }

// log 写一条日志：页面上显示，也记进 SQLite。
func (d *device) log(session int64, kind, detail string, table int) {
	d.s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: kind, Detail: detail, Table: table})
	if r := d.rec(); r != nil && session != 0 {
		r.Log(session, recorder.Event{Kind: kind, Detail: detail, Window: table})
	}
	d.s.changed()
}

// connect 按 cfg 建立连接，原有连接先断开。失败时不自动重试。
func (d *device) connect(cfg ConnConfig) error {
	mode, err := cfg.normalize()
	if err != nil {
		return err
	}
	d.disconnect()
	ctx, cancel := context.WithCancel(d.s.ctx)
	target := cfg.Target
	if cfg.Simulator {
		addr, err := startSimulator(ctx, mode)
		if err != nil {
			cancel()
			return fmt.Errorf("内置模拟器启动失败：%w", err)
		}
		target = addr
	}
	var session int64
	if r := d.rec(); r != nil {
		session, _ = r.StartSession(mode, cfg.describe(target), 0)
	}
	d.mu.Lock()
	d.cfg, d.mode, d.target, d.session, d.ctx, d.cancel = cfg, mode, target, session, ctx, cancel
	d.state, d.err, d.since = stateConnecting, "", time.Now()
	d.mu.Unlock()
	d.s.changed()

	client, local, remote, err := d.dial(ctx)
	d.mu.Lock()
	if ctx.Err() != nil { // 连接过程中被断开或换了连接
		d.mu.Unlock()
		if client != nil {
			client.Close()
		}
		return errors.New("连接已取消")
	}
	if err != nil {
		d.state, d.err, d.cancel, d.session = stateDisconnected, dialText(mode, err), nil, 0
		d.mu.Unlock()
		cancel()
		d.log(session, recorder.EventConnectFail, "连接失败："+dialText(mode, err), 0)
		if r := d.rec(); r != nil && session != 0 {
			r.EndSession(session)
		}
		return err
	}
	d.client, d.local, d.remote, d.state, d.since = client, local, remote, stateConnected, time.Now()
	d.mu.Unlock()
	d.log(session, recorder.EventConnect, "连接建立："+d.endpoints(local, remote), 0)
	return nil
}

func (d *device) endpoints(local, remote string) string {
	if local == "" {
		return d.cfg.describe(d.target)
	}
	return fmt.Sprintf("本机 %s → 设备 %s", local, remote)
}

// dial 按当前参数打开连接并创建客户端。
func (d *device) dial(ctx context.Context) (c *modbus.Client, local, remote string, err error) {
	d.mu.Lock()
	cfg, mode, target, session := d.cfg, d.mode, d.target, d.session
	d.mu.Unlock()
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	var t modbus.Transport
	if mode.Serial() {
		t, err = transport.OpenSerial(transport.SerialConfig{Port: cfg.Port, BaudRate: cfg.Baud, DataBits: cfg.DataBits, Parity: cfg.Parity, StopBits: cfg.StopBits})
	} else {
		var conn net.Conn
		conn, err = transport.DialTCP(ctx, target, max(timeout, 3*time.Second))
		if err == nil {
			t, local, remote = conn, conn.LocalAddr().String(), conn.RemoteAddr().String()
		}
	}
	if err != nil {
		return nil, "", "", err
	}
	opts := modbus.DefaultOptions(mode)
	opts.Timeout = timeout
	opts.Observer = modbus.ObserverFunc(func(p modbus.Packet) {
		if p.Status == modbus.StatusConnectionError && p.Err != nil {
			d.mu.Lock()
			d.netErr = p.Err
			d.mu.Unlock()
		}
		if r := d.rec(); r != nil && session != 0 {
			r.Record(session, p)
		}
		d.s.packets.add(p)
	})
	return modbus.NewClient(t, opts), local, remote, nil
}

// linkDown 在请求返回连接错误时调用：关闭这个客户端，开始重连。别的请求已经处理过时忽略。
func (d *device) linkDown(c *modbus.Client, cause error) {
	d.mu.Lock()
	if d.client != c || d.cancel == nil {
		d.mu.Unlock()
		return
	}
	why, detail := cause.Error(), "连接断开："+cause.Error()
	if k := transport.CloseKindOf(d.netErr); k != transport.CloseUnknown {
		why = k.Label()
		detail = fmt.Sprintf("连接断开：%s。%s原始错误：%v", why, k.Explain(), d.netErr)
	}
	d.client, d.state, d.err, d.since, d.netErr = nil, stateReconnecting, why, time.Now(), nil
	session, ctx := d.session, d.ctx
	d.mu.Unlock()
	c.Close()
	d.log(session, recorder.EventDisconnect, detail, 0)
	go d.reconnect(ctx)
}

// dialText 是连接失败的原因：TCP 连接被拒绝、超时说成中文；串口的错误 transport 已经说明白了。
func dialText(mode modbus.Mode, err error) string {
	if mode.Serial() {
		return err.Error()
	}
	return transport.DialErrText(err)
}

func (d *device) reconnect(parent context.Context) {
	for {
		select {
		case <-parent.Done():
			return
		case <-time.After(reconnectEvery):
		}
		d.mu.Lock()
		if d.state != stateReconnecting {
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		ctx, stop := context.WithTimeout(parent, 10*time.Second)
		client, local, remote, err := d.dial(ctx)
		stop()
		d.mu.Lock()
		if d.state != stateReconnecting { // 期间用户断开了
			d.mu.Unlock()
			if client != nil {
				client.Close()
			}
			return
		}
		if err != nil {
			d.err = dialText(d.mode, err)
			d.mu.Unlock()
			d.s.changed()
			continue
		}
		d.client, d.local, d.remote, d.state, d.err, d.since = client, local, remote, stateConnected, "", time.Now()
		session := d.session
		d.mu.Unlock()
		d.log(session, recorder.EventReconnect, "重连成功："+d.endpoints(local, remote), 0)
		return
	}
}

// disconnect 断开连接、停止重连和内置模拟器，结束这次记录会话。
func (d *device) disconnect() {
	d.mu.Lock()
	client, cancel, session, state := d.client, d.cancel, d.session, d.state
	d.client, d.cancel, d.session, d.state, d.err, d.since = nil, nil, 0, stateDisconnected, "", time.Now()
	d.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if client != nil {
		client.Close()
	}
	if state != stateDisconnected {
		d.s.logs.add(logView{Time: time.Now().Format("15:04:05"), Kind: "DISCONNECT", Detail: "已断开"})
	}
	if r := d.rec(); r != nil && session != 0 {
		r.EndSession(session)
	}
	d.s.changed()
}

// startSimulator 在本机随机端口运行换热站模拟器，数据每秒变化，ctx 结束时停止。
func startSimulator(ctx context.Context, mode modbus.Mode) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	store := simulator.HeatStation()
	srv := simulator.NewServer(mode, 1, store)
	go simulator.RunHeatStation(ctx, store)
	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	return ln.Addr().String(), nil
}

type connView struct {
	State  string     `json:"state"`
	Config ConnConfig `json:"config"`
	Desc   string     `json:"desc"`
	Error  string     `json:"error"`
	Since  string     `json:"since"`
	Local  string     `json:"local"`
	Remote string     `json:"remote"`
}

func (d *device) view() connView {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := connView{State: d.state, Config: d.cfg, Error: d.err, Local: d.local, Remote: d.remote}
	if d.state != stateDisconnected {
		v.Desc = string(d.mode) + " · " + d.cfg.describe(d.target)
	}
	if !d.since.IsZero() {
		v.Since = d.since.Format("15:04:05")
	}
	return v
}

// ---- 报文和日志 ----

const (
	maxPackets = 2000
	maxLogs    = 200
)

type packetView struct {
	Seq      uint64  `json:"seq"`
	Time     string  `json:"time"`
	Dir      string  `json:"dir"`
	Req      uint64  `json:"req"`
	Slave    int     `json:"slave"`
	Function int     `json:"fn"`
	Address  int     `json:"addr"`
	Count    int     `json:"count"`
	Data     string  `json:"data"` // 十六进制；ASCII 模式是帧本身的字符
	Status   string  `json:"status"`
	RTT      float64 `json:"rtt"` // ms，仅结果行
	Error    string  `json:"error,omitempty"`
}

// packetRing 保存最近的收发，页面打开时先取这些，之后靠推送。
type packetRing struct {
	mu   sync.Mutex
	seq  uint64
	list []packetView
}

func (r *packetRing) add(p modbus.Packet) {
	v := packetView{Time: p.Time.Format("15:04:05.000"), Dir: string(p.Dir), Req: p.RequestID, Slave: int(p.Slave),
		Function: int(p.Function), Address: int(p.Address), Count: int(p.Count), Status: string(p.Status),
		RTT: float64(p.RTT.Microseconds()) / 1000}
	if p.Mode.IsASCII() {
		v.Data = strings.TrimSuffix(string(p.Raw), "\r\n")
	} else {
		v.Data = hexBytes(p.Raw)
	}
	if p.Err != nil {
		v.Error = p.Err.Error()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	v.Seq = r.seq
	if len(r.list) == maxPackets {
		r.list = append(r.list[:0], r.list[maxPackets/4:]...) // 一次挪出四分之一，不必每条都搬
	}
	r.list = append(r.list, v)
}

func (r *packetRing) last() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// since 返回序号大于 seq 的报文。
func (r *packetRing) since(seq uint64) []packetView {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := len(r.list)
	for i > 0 && r.list[i-1].Seq > seq {
		i--
	}
	return append([]packetView{}, r.list[i:]...) // 没有报文时也是 []，不是 null
}

func hexBytes(b []byte) string {
	const digits = "0123456789ABCDEF"
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, len(b)*3-1)
	for i, v := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[v>>4], digits[v&0x0F])
	}
	return string(out)
}

type logView struct {
	Time   string `json:"time"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Table  int    `json:"table,omitempty"`
}

type logRing struct {
	mu   sync.Mutex
	list []logView
}

func (r *logRing) add(v logView) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.list) == maxLogs {
		r.list = append(r.list[:0], r.list[1:]...)
	}
	r.list = append(r.list, v)
}

func (r *logRing) all() []logView {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]logView{}, r.list...)
}
