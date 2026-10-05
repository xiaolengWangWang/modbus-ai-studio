package ui

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/transport"
)

// 连接保持：TCP 类连接中途断开后自动重连，并按断开发生的时机分析原因——连上就断、某条请求后立刻断、
// 空闲一段时间后断、用了一阵后随机断。串口断开多半是 USB 转 485 被拔掉，不自动重连，只提示。
// 网线被拔时 TCP 不会报错，表现为超时，走超时的分析。
var (
	reconnectDelays = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	stableFor       = 30 * time.Second       // 连接活过这么久，下次断开从 1 s 重新退避
	idleMin         = 10 * time.Second       // 请求前空闲超过这么久，才考虑“空闲超时”
	acceptProbe     = 300 * time.Millisecond // 重连后先等这么久，看设备是不是连上就关
)

type lossKind int

const (
	lossOnConnect    lossKind = iota // 连上就断，还没发请求
	lossAfterRequest                 // 发出的请求在这条连接上从没成功过，发完就断
	lossIdle                         // 空闲一段时间后的第一条请求遇到断开
	lossRandom                       // 断开前的请求平时正常
	lossSerial                       // 串口读写出错
)

// linkState 记录一条连接（每次重连都是新的一条）上的活动，由收发回调更新。
type linkState struct {
	mu     sync.Mutex
	up     time.Time
	lastTX time.Time
	gap    time.Duration   // 最后一条请求发出前空闲了多久
	last   modbus.Packet   // 最后一条请求
	ok     map[reqKey]bool // 这条连接上成功过（含异常响应）的请求
	lost   bool            // 已经报告过断开，同一条连接上后面的连接错误不再处理
}

func newLink() *linkState { return &linkState{up: time.Now(), ok: map[reqKey]bool{}} }

// observe 更新连接活动，返回 true 表示这是这条连接上第一次出现连接错误。
func (l *linkState) observe(p modbus.Packet) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.Dir == modbus.DirTX {
		ref := l.lastTX
		if ref.IsZero() {
			ref = l.up
		}
		l.gap, l.lastTX, l.last = p.Time.Sub(ref), p.Time, p
	}
	if p.Status == modbus.StatusSuccess || p.Status == modbus.StatusException {
		l.ok[reqKey{p.Slave, p.Function, p.Address}] = true
	}
	if p.Status == modbus.StatusConnectionError && !l.lost {
		l.lost = true
		return true
	}
	return false
}

// lossEvent 是一次断开。
type lossEvent struct {
	at     time.Time
	kind   lossKind
	req    modbus.Packet // 断开前最后一条请求；连上就断时为空
	gap    time.Duration
	uptime time.Duration
	err    string
}

func (e lossEvent) key() reqKey { return reqKey{e.req.Slave, e.req.Function, e.req.Address} }

func classifyLoss(mode modbus.Mode, l *linkState, errText string) lossEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := lossEvent{at: time.Now(), req: l.last, gap: l.gap, uptime: time.Since(l.up), err: errText}
	switch ok := l.ok[e.key()]; {
	case mode.Serial():
		e.kind = lossSerial
	case l.last.Dir == "":
		e.kind = lossOnConnect
	case !ok:
		e.kind = lossAfterRequest
	case e.gap >= idleMin:
		e.kind = lossIdle
	default:
		e.kind = lossRandom
	}
	return e
}

func reqDesc(p modbus.Packet) string {
	return fmt.Sprintf("Slave %d · %s · %s", p.Slave, p.Function, refSpan(modbus.AreaOf(p.Function), p.Address, int(p.Count)))
}

func roundDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// describe 是写进报文库和状态说明的一句话。
func (e lossEvent) describe() string {
	switch e.kind {
	case lossOnConnect:
		return "连接建立后 " + roundDur(e.uptime) + " 就被设备关闭，还没发请求"
	case lossSerial:
		return "串口读写出错：" + e.err
	}
	s := fmt.Sprintf("发送 %s 后连接断开（%s）", reqDesc(e.req), e.err)
	return s + fmt.Sprintf("，发送前空闲 %s，连接已用 %s", roundDur(e.gap), roundDur(e.uptime))
}

func dialErrText(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "连接被拒绝：设备的 Modbus 服务没开，或端口不对"
	case errors.Is(err, os.ErrDeadlineExceeded), isNetTimeout(err):
		return "连接超时：设备不在线或网络不通"
	}
	return err.Error()
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// probeClosed 连上后先读一会儿：设备连接数已满、有 IP 白名单时，往往接受连接后马上关闭。
func probeClosed(t modbus.Transport, wait time.Duration) (time.Duration, bool) {
	start := time.Now()
	_ = t.SetReadDeadline(start.Add(wait))
	defer t.SetReadDeadline(time.Time{})
	var b [1]byte
	_, err := t.Read(b[:])
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) || isNetTimeout(err) {
		return 0, false
	}
	return time.Since(start), true
}

// onPacket 是每条连接自己的收发回调：除了公共处理，还跟踪连接活动，第一次出现连接错误时交给界面线程处理断开。
func (ws *Workspace) onPacket(s *session, p modbus.Packet) {
	ws.OnPacket(p)
	if l := s.link.Load(); l != nil && l.observe(p) {
		errText := "连接错误"
		if p.Err != nil {
			errText = p.Err.Error()
		}
		uiDo(func() { ws.connLost(s, l, errText) })
	}
}

func (ws *Workspace) clientOptions(s *session, timeout time.Duration) modbus.Options {
	return modbus.Options{Mode: s.mode, Timeout: timeout, Observer: modbus.ObserverFunc(func(p modbus.Packet) { ws.onPacket(s, p) })}
}

// connLost 处理一次断开：停止轮询（不再刷连接错误），分析原因，TCP 类自动重连。
func (ws *Workspace) connLost(s *session, l *linkState, errText string) {
	if ws.session != s || s.link.Load() != l || s.lost != nil {
		return // 旧连接迟到的回调
	}
	e := classifyLoss(s.mode, l, errText)
	ws.addLoss(s, e)
	for _, w := range ws.windows {
		w.halt()
	}
	s.client.Close()
	if !s.mode.Serial() {
		start := s.backoff
		if e.uptime >= stableFor {
			start = 0
		}
		go ws.reconnectLoop(s, start)
	}
	ws.refreshAll()
}

func (ws *Workspace) addLoss(s *session, e lossEvent) {
	again := s.lost != nil && s.lost.kind == lossOnConnect && e.kind == lossOnConnect
	s.lost = &e
	s.losses = append(s.losses, e)
	if again || ws.session != s {
		return // 重连时一连上就被关闭，每次重试都一样，日志只记第一次
	}
	var tx, res modbus.Packet
	if e.kind != lossOnConnect {
		tx, res, _ = ws.ring.exchange(func(modbus.Packet) bool { return true }) // 断开前最后一条请求
	}
	ws.addLog(newLogEntry(recorder.EventDisconnect, 0, e.describe(), diagnosisLines(ws.lossDiagnosis()), tx, res, ws.points), s.recID)
}

func (ws *Workspace) refreshAll() {
	for _, w := range ws.windows {
		w.refresh()
	}
	ws.refreshStatus()
}

// reconnectLoop 按 1 / 2 / 5 / 10 s 退避重连，直到成功或用户断开。连上后先探一下是不是马上被关。
func (ws *Workspace) reconnectLoop(s *session, attempt int) {
	for ; ; attempt++ {
		d := reconnectDelays[min(attempt, len(reconnectDelays)-1)]
		uiDo(func() {
			s.tries++
			s.retryAt = time.Now().Add(d)
			ws.refreshStatus()
		})
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(d):
		}
		t, err := transport.DialTCP(s.ctx, s.target, 3*time.Second)
		if s.ctx.Err() != nil {
			if err == nil {
				t.Close()
			}
			return
		}
		if err != nil {
			msg := dialErrText(err)
			uiDo(func() {
				if msg != s.dialErr && ws.session == s { // 同样的失败只记第一次
					cfg := connConfig{mode: s.mode, target: s.target}
					ws.addLog(newLogEntry(recorder.EventConnectFail, 0, fmt.Sprintf("第 %d 次重连失败 · %s", s.tries, msg),
						connectFailCause(cfg, err), modbus.Packet{}, modbus.Packet{}, nil), s.recID)
				}
				s.dialErr = msg
				ws.refreshStatus()
			})
			continue
		}
		if after, closed := probeClosed(t, acceptProbe); closed {
			t.Close()
			uiDo(func() {
				if ws.session == s {
					ws.addLoss(s, lossEvent{at: time.Now(), kind: lossOnConnect, uptime: after})
					ws.refreshAll()
				}
			})
			continue
		}
		uiDo(func() { ws.relink(s, t, attempt) })
		return
	}
}

// relink 换上新连接，恢复轮询。
func (ws *Workspace) relink(s *session, t modbus.Transport, attempt int) {
	if ws.session != s {
		t.Close()
		return
	}
	down := time.Since(s.lost.at)
	s.link.Store(newLink())
	s.client = modbus.NewClient(t, ws.clientOptions(s, ws.timeout))
	s.lost, s.dialErr, s.retryAt = nil, "", time.Time{}
	s.reconnects++
	s.backoff = attempt + 1
	ws.addLog(logEntry{Event: recorder.Event{Time: time.Now(), Kind: recorder.EventReconnect,
		Detail: fmt.Sprintf("第 %d 次重连成功，断开了 %s", s.reconnects, roundDur(down))}}, s.recID)
	for _, w := range ws.windows {
		w.start()
	}
	ws.refreshAll()
}

// lossDiagnosis 是连接断开期间读取窗口里显示的分析和一键处理。
func (ws *Workspace) lossDiagnosis() diagnosis {
	s := ws.session
	e := s.lost
	if e.kind == lossSerial {
		return diagnosis{Text: "串口断开", Action: "重新连接", Do: ws.reconnect,
			Hint: "串口读写出错（" + e.err + "）：USB 转 485 可能被拔掉或驱动掉线。插回后点“重新连接”。"}
	}
	dg := diagnosis{Text: "连接断开，正在自动重连（状态栏有进度）"}
	same := 0
	for _, x := range s.losses {
		if x.kind == lossAfterRequest && x.key() == e.key() {
			same++
		}
	}
	switch e.kind {
	case lossOnConnect:
		dg.Hint = "设备在连接建立后马上关闭了连接，还没发请求：① 设备的连接数可能已满（很多设备只允许 1–4 个连接，组态软件、别的上位机正占着）；" +
			"② 设备或防火墙有 IP 白名单；③ 这个端口不是 Modbus 服务。"
	case lossAfterRequest:
		switch {
		case same < 2:
			dg.Hint = fmt.Sprintf("发出 %s 后连接被断开。如果每次都这样，说明设备不接受这条请求。", reqDesc(e.req))
		case ws.evidence.okAt.Load() == 0:
			dg.Hint = "每次发出请求设备就断开，一次正常响应都没有：多半是协议格式不对（Modbus TCP 设备收到 RTU / ASCII 帧会直接断开连接），" +
				"也可能 Unit ID 不对（有的网关对不认识的站号直接断开）。"
			dg.Action, dg.Do = "识别协议", ws.detectProtocol
		default:
			dg.Hint = fmt.Sprintf("每次发 %s 设备都会断开，其他请求正常：设备不接受这个 Unit ID、功能码或地址范围（有的设备不回异常，直接断开）。"+
				"先暂停这条请求，其他窗口照常工作。", reqDesc(e.req))
			dg.Action, dg.Do = "暂停这条请求", func() {
				for _, w := range ws.windows {
					if (reqKey{w.def.Slave, w.def.Function, w.def.Start}) == e.key() {
						w.setPaused(true)
					}
				}
			}
		}
	case lossIdle:
		scan := max(e.gap/4, 200*time.Millisecond).Truncate(100 * time.Millisecond)
		dg.Hint = fmt.Sprintf("连接空闲 %s 后，第一条请求就遇到断开：设备有空闲超时，没有通信一段时间就主动断开。把扫描周期缩短，或加大设备的空闲超时。", roundDur(e.gap))
		dg.Action, dg.Do = fmt.Sprintf("扫描周期改为 %s", roundDur(scan)), func() {
			for _, w := range append([]*readWindow(nil), ws.windows...) {
				if w.def.Scan > scan {
					ws.redefine(w, func(d *readDef) { d.Scan = scan })
				}
			}
		}
	case lossRandom:
		dg.Hint = fmt.Sprintf("连接用了 %s 后断开，断开前的请求平时正常：更像是网络不稳（网线、交换机、无线网桥）或设备重启。", roundDur(e.uptime))
		if n := len(s.losses); n >= 3 {
			span := s.losses[n-1].at.Sub(s.losses[0].at)
			dg.Hint += fmt.Sprintf("本次连接已断开 %d 次，平均 %s 一次。", n, roundDur(span/time.Duration(n-1)))
		}
	}
	if s.dialErr != "" {
		dg.Hint += "\n上次重连：" + s.dialErr
	}
	if s.sim != nil && dg.Action == "识别协议" {
		dg.Action, dg.Do = "", nil
	}
	return dg
}

// linkStatus 是状态栏里的连接状态。
func (s *session) linkStatus() string {
	if s.lost == nil {
		if s.reconnects > 0 {
			return fmt.Sprintf(" · 已重连 %d 次", s.reconnects)
		}
		return ""
	}
	if s.lost.kind == lossSerial {
		return " · 串口断开，插回后点“重新连接”"
	}
	text := fmt.Sprintf(" · 正在重连（第 %d 次）", s.tries)
	if wait := time.Until(s.retryAt); wait > 0 {
		text = fmt.Sprintf(" · %d s 后第 %d 次重连", int(math.Ceil(wait.Seconds())), s.tries)
	}
	if s.dialErr != "" {
		text += " · 上次：" + s.dialErr
	}
	return text
}
