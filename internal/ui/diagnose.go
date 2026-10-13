package ui

import (
	"slices"

	"modbus-ai-studio/internal/diagnose"
	"modbus-ai-studio/internal/modbus"
)

// reqKey 标识一种请求，断线分析按它把出错的请求归到读取窗口（link.go）。
type reqKey struct {
	slave byte
	fn    modbus.FunctionCode
	addr  uint16
}

// diagnosis 是读取窗口错误行下面的自动分析：错误本身、可能原因，以及可选的一键处理。
type diagnosis struct {
	Text   string
	Hint   string
	Action string // 按钮文字，空表示没有一键处理
	Do     func()
}

const foreignAdvice = diagnose.ForeignAdvice

// diagnose 分析读取窗口的错误：规则在 internal/diagnose，和 Web 版、modbus-cli 共用；这里把一键处理接到窗口上。
func (ws *Workspace) diagnose(w *readWindow, err error) diagnosis {
	s := ws.session
	if s == nil {
		return diagnosis{Text: "未连接"}
	}
	d := w.def
	r := diagnose.Analyze(diagnose.Request{Slave: d.Slave, Function: d.Function, Start: d.Start, Qty: d.Qty, Scan: d.Scan},
		diagnose.Context{Mode: s.mode, Timeout: ws.timeout, Simulator: s.sim != nil, Evidence: &ws.evidence}, err)
	dg := diagnosis{Text: r.Text, Hint: r.Hint, Action: r.Label}
	switch r.Action {
	case diagnose.SetTimeout:
		dg.Do = func() { ws.setTimeout(r.Timeout) }
	case diagnose.UseFC03:
		dg.Do = func() { ws.redefine(w, func(d *readDef) { d.Function = modbus.FuncReadHoldingRegisters }) }
	case diagnose.UseFC04:
		dg.Do = func() { ws.redefine(w, func(d *readDef) { d.Function = modbus.FuncReadInputRegisters }) }
	case diagnose.ProbeRange:
		dg.Do = func() { ws.probeRange(w) }
	case diagnose.HalveQty:
		dg.Do = func() { ws.redefine(w, func(d *readDef) { d.Qty = max(1, d.Qty/2) }) }
	case diagnose.DoubleScan:
		dg.Do = func() { ws.redefine(w, func(d *readDef) { d.Scan *= 2 }) }
	case diagnose.ScanSlaves:
		dg.Do = ws.scanSlavesDialog
	case diagnose.DetectProtocol:
		dg.Do = ws.detectProtocol
	case diagnose.ScanSerial:
		dg.Do = ws.scanSerialDialog
	case diagnose.Reconnect:
		dg.Do = ws.reconnect
	default:
		dg.Action = ""
	}
	return dg
}

// suggestPointOrder 检查按点表显示的浮点点：当前字节序下多数不合理、而恰好有一种字节序全部合理时，
// 返回当前和建议的字节序（都用 32 位写法）。窗口里的浮点点字节序不一致时不判断。
func suggestPointOrder(pts pointTable, d readDef, regs []uint16) (cur, sug modbus.ByteOrder, ok bool) {
	type sample struct {
		t    modbus.DataType
		regs []uint16
	}
	var ss []sample
	for i := range regs {
		p, found := pts.Get(d.area(), d.Start+uint16(i))
		if !found || !p.Type.Float() || i+p.Regs() > len(regs) || !slices.ContainsFunc(regs[i:i+p.Regs()], func(r uint16) bool { return r != 0 }) {
			continue
		}
		o := p.Order.For(modbus.TypeFloat32)
		if cur != "" && o != cur {
			return "", "", false
		}
		cur = o
		ss = append(ss, sample{p.Type, regs[i : i+p.Regs()]})
	}
	good := func(o modbus.ByteOrder) int {
		n := 0
		for _, s := range ss {
			if v, err := modbus.DecodeRaw(s.t, o.For(s.t), s.regs); err == nil && isPlausible(s.t, v) {
				n++
			}
		}
		return n
	}
	if len(ss) == 0 || good(cur)*2 > len(ss) {
		return "", "", false
	}
	var found []modbus.ByteOrder
	for _, o := range modbus.Orders32 {
		if o != cur && good(o) == len(ss) {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		return "", "", false
	}
	return cur, found[0], true
}

// suggestFloatOrder 检查 FLOAT32 / FLOAT64 显示：当前字节序下多数非零值不合理、而恰好有一种字节序全部合理时，
// 返回那种字节序。这是确定性判断，不依赖 AI。
func suggestFloatOrder(dt modbus.DataType, regs []uint16, cur modbus.ByteOrder) (modbus.ByteOrder, bool) {
	var pairs [][]uint16
	n := dt.Registers()
	for i := 0; i+n <= len(regs); i += n {
		for _, r := range regs[i : i+n] {
			if r != 0 {
				pairs = append(pairs, regs[i:i+n])
				break
			}
		}
	}
	if len(pairs) == 0 {
		return "", false
	}
	good := func(o modbus.ByteOrder) int {
		n := 0
		for _, p := range pairs {
			if v, err := modbus.DecodeRaw(dt, o, p); err == nil && isPlausible(dt, v) {
				n++
			}
		}
		return n
	}
	if good(cur)*2 > len(pairs) {
		return "", false
	}
	var found []modbus.ByteOrder
	for _, o := range dt.Orders() {
		if o != cur && good(o) == len(pairs) {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
