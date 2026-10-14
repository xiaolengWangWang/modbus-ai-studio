package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/control"
	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/eta"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/slavescan"
	"modbus-ai-studio/internal/transport"
)

// ---- 地址和数据类型 ----

var fcAreas = map[int]modbus.Area{
	1: modbus.AreaCoils, 2: modbus.AreaDiscreteInputs, 3: modbus.AreaHoldingRegisters, 4: modbus.AreaInputRegisters,
	5: modbus.AreaCoils, 6: modbus.AreaHoldingRegisters, 16: modbus.AreaHoldingRegisters,
}

// address 解析地址，返回协议地址和数据区。-fc 指定了功能码时按它的数据区取地址；否则按地址写法，
// 原始 Offset 按保持寄存器。有歧义时按第一种解释执行，并说明另一种写法（设计文档 6.3：不静默猜测）。
func address(s string, fcs ...int) (uint16, modbus.Area) {
	cands, err := modbus.ParseAddress(s)
	if err != nil {
		fail("%v", err)
	}
	if *fcArg != 0 {
		area, ok := fcAreas[*fcArg]
		if !ok || !slices.Contains(fcs, *fcArg) {
			fail("这条命令的 -fc 只能是 %v", fcs)
		}
		for _, c := range cands {
			if c.Area == area || c.Area == modbus.AreaNone {
				return c.Offset, area
			}
		}
		fail("地址 %s 是 %s 区，和 -fc %d 对不上", s, cands[0].Area.Prefix(), *fcArg)
	}
	c := cands[0]
	if len(cands) > 1 && !*jsonOut {
		fmt.Fprintf(os.Stderr, "注意：%s 有 %d 种解释，已按“%s → Offset %d”执行；如果指的是原始 Offset %d，请写成 0x%04X。\n",
			s, len(cands), c.Label, c.Offset, cands[1].Offset, cands[1].Offset)
	}
	if c.Area == modbus.AreaNone {
		return c.Offset, modbus.AreaHoldingRegisters
	}
	return c.Offset, c.Area
}

func dataType() modbus.DataType {
	t := modbus.DataType(strings.ToUpper(*typ))
	switch t {
	case modbus.TypeInt16, modbus.TypeUint16, modbus.TypeInt32, modbus.TypeUint32, modbus.TypeFloat32,
		modbus.TypeInt64, modbus.TypeUint64, modbus.TypeFloat64:
		return t
	}
	fail("不支持的数据类型 %q", *typ)
	return ""
}

func byteOrder(dt modbus.DataType) modbus.ByteOrder {
	if *order == "" {
		return modbus.OrderAB.For(dt)
	}
	o := modbus.ByteOrder(strings.ToUpper(*order))
	if dt.Registers() == 4 {
		o = o.For(dt)
	}
	if _, err := modbus.EncodeRaw(dt, o, 0); err != nil {
		fail("%s 不能用字节序 %s", dt, o)
	}
	return o
}

// ---- read ----

type readValue struct {
	Address   string   `json:"address"`
	Offset    uint16   `json:"offset"`
	Registers []string `json:"registers,omitempty"` // 十六进制，寄存器区才有
	Value     string   `json:"value"`               // 工程值；线圈、离散输入是 0 或 1
	Hint      string   `json:"hint,omitempty"`
}

type readResult struct {
	OK        bool        `json:"ok"`
	Mode      string      `json:"mode"`
	Slave     uint        `json:"slave"`
	Function  int         `json:"function"`
	Start     string      `json:"start"`
	Count     int         `json:"count"`
	Type      string      `json:"type,omitempty"`
	Order     string      `json:"order,omitempty"`
	ElapsedMS int64       `json:"elapsed_ms"`
	Values    []readValue `json:"values"`
}

func runRead(ctx context.Context, addr, count string) {
	off, area := address(addr, 1, 2, 3, 4)
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		fail("数量应为正整数")
	}
	bits := area == modbus.AreaCoils || area == modbus.AreaDiscreteInputs
	var dt modbus.DataType
	var bo modbus.ByteOrder
	if !bits {
		dt = dataType()
		bo = byteOrder(dt)
	}
	c := connect(ctx, 1)
	defer c.Close()
	fc := area.ReadFunction()
	start := time.Now()
	rq := modbus.Request{Slave: byte(*slave), Function: fc, Address: off, Quantity: uint16(n)}
	resp, err := c.Do(ctx, rq)
	if err != nil {
		explain(rq, err)
	}
	res := readResult{OK: true, Mode: modeName(c.Mode()), Slave: *slave, Function: int(fc), Start: modbus.Reference(area, off), Count: n,
		ElapsedMS: time.Since(start).Milliseconds(), Values: []readValue{}}
	if bits {
		for i, b := range resp.Bits {
			v := "0"
			if b {
				v = "1"
			}
			res.Values = append(res.Values, readValue{Address: modbus.Reference(area, off+uint16(i)), Offset: off + uint16(i), Value: v})
		}
	} else {
		res.Type, res.Order = string(dt), string(bo)
		w := dt.Registers()
		for i := 0; i+w <= len(resp.Registers); i += w {
			regs := resp.Registers[i : i+w]
			v := readValue{Address: modbus.Reference(area, off+uint16(i)), Offset: off + uint16(i)}
			for _, r := range regs {
				v.Registers = append(v.Registers, fmt.Sprintf("%04X", r))
			}
			if exact, err := modbus.FormatInt(dt, bo, regs); err == nil && *scale == 1 {
				v.Value = exact // 整型精确显示，64 位不经过 float64
			} else if raw, err := modbus.DecodeRaw(dt, bo, regs); err == nil {
				v.Value = formatEng(modbus.Scaling{Scale: *scale}.Engineering(raw), dt)
				if dt == modbus.TypeFloat32 {
					if sug, ok := modbus.SuggestByteOrder(bo, regs); ok {
						v.Hint = fmt.Sprintf("数值不合理，按 %s 解码更可能正确", sug)
					}
				}
			} else {
				fail("%v", err)
			}
			res.Values = append(res.Values, v)
		}
	}
	output(res, func() {
		fmt.Printf("%s · Slave %d · FC%02d · %s × %d · %d ms\n\n", res.Mode, res.Slave, res.Function, res.Start, n, res.ElapsedMS)
		if bits {
			fmt.Printf("%-8s %-7s %s\n", "地址", "Offset", "值")
		} else {
			fmt.Printf("%-8s %-7s %-12s %s %s\n", "地址", "Offset", "寄存器", dt, bo)
		}
		for _, v := range res.Values {
			val := v.Value
			if v.Hint != "" {
				val += "（" + v.Hint + "）"
			}
			if bits {
				fmt.Printf("%-8s %-7d %s\n", v.Address, v.Offset, val)
			} else {
				fmt.Printf("%-8s %-7d %-12s %s\n", v.Address, v.Offset, strings.Join(v.Registers, " "), val)
			}
		}
	})
}

// formatEng 格式化工程值：FLOAT32 按单精度显示（45.2 而不是 45.200001），
// 整型按 Scale 的小数位数显示（132 × 0.1 显示 13.2）。
func formatEng(v float64, dt modbus.DataType) string {
	if dt == modbus.TypeFloat32 && *scale == 1 {
		return strconv.FormatFloat(v, 'g', -1, 32)
	}
	if dt == modbus.TypeFloat64 && *scale == 1 {
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	s := strconv.FormatFloat(*scale, 'f', -1, 64)
	decimals := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimals = len(s) - i - 1
	}
	if dt.Float() {
		decimals += 3
	}
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// ---- write ----

type writeResult struct {
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Address   string   `json:"address"`
	Function  int      `json:"function"`
	Value     string   `json:"value"`               // 实际写入的值（取整后）
	Rounded   bool     `json:"rounded,omitempty"`   // 按类型取整过
	Registers []string `json:"registers,omitempty"` // 写入的寄存器，十六进制
	Request   string   `json:"request"`             // 请求帧（Modbus TCP 的事务号按 1 显示）
	Readback  string   `json:"readback"`
	Verified  bool     `json:"verified"` // 回读与写入的值一致
}

// runWrite 写入后立即回读比较（控制验证的最小形态，完整流程见设计文档 11.4）。线圈写 0 / 1，寄存器按 -type / -order 编码。
func runWrite(ctx context.Context, addr, text string) {
	off, area := address(addr, 5, 6, 16)
	if !area.Writable() {
		fail("%s 区只读，不能写入", area.Prefix())
	}
	res := writeResult{Address: modbus.Reference(area, off)}
	req := modbus.Request{Slave: byte(*slave), Address: off}
	var show func(regs []uint16) string
	if area == modbus.AreaCoils {
		on, ok := map[string]bool{"1": true, "0": false, "true": true, "false": false, "on": true, "off": false}[strings.ToLower(strings.TrimSpace(text))]
		if !ok {
			fail("线圈的值写 0 或 1（也可写 on、off、true、false）")
		}
		req.Function, req.Bits = modbus.FuncWriteSingleCoil, []bool{on}
		res.Value = map[bool]string{true: "1", false: "0"}[on]
	} else {
		dt := dataType()
		bo := byteOrder(dt)
		t := control.Target{Type: dt, ReadOrder: bo, Scaling: modbus.Scaling{Scale: *scale}}
		regs, _, rounded, err := control.EncodeText(t, text)
		if err != nil {
			fail("%v", err)
		}
		show = func(regs []uint16) string {
			if s, err := modbus.FormatInt(dt, bo, regs); err == nil && *scale == 1 {
				return s
			}
			raw, _ := modbus.DecodeRaw(dt, bo, regs)
			return strconv.FormatFloat(modbus.Scaling{Scale: *scale}.Engineering(raw), 'g', -1, 64)
		}
		req.Function, req.Values = modbus.FuncWriteMultipleRegisters, regs
		switch {
		case *fcArg == 6 && len(regs) > 1:
			fail("%s 占 %d 个寄存器，不能用 06 单个写，用 -fc 16", dt, len(regs))
		case *fcArg == 6 || *fcArg == 0 && len(regs) == 1:
			req.Function = modbus.FuncWriteSingleRegister
		}
		res.Value, res.Rounded = show(regs), rounded
		for _, r := range regs {
			res.Registers = append(res.Registers, fmt.Sprintf("%04X", r))
		}
	}
	res.Function = int(req.Function)
	c := connect(ctx, 1)
	defer c.Close()
	pdu, err := req.PDU()
	if err != nil {
		fail("%v", err)
	}
	res.Request = hexs(modbus.EncodeADU(c.Mode(), req.Slave, 1, pdu))
	if !*jsonOut {
		fmt.Printf("目标值   %s\n", strings.TrimSpace(text))
		if res.Rounded {
			fmt.Printf("注意     取整后实际写入 %s\n", res.Value)
		}
		if len(res.Registers) > 0 {
			fmt.Printf("寄存器   %s\n", strings.Join(res.Registers, " "))
		}
		fmt.Printf("请求     %s\n\n", res.Request)
	}
	if _, err := c.Do(ctx, req); err != nil {
		explain(req, err)
	}
	if !*jsonOut {
		fmt.Println("协议写入：SUCCESS")
	}
	back := modbus.Request{Slave: req.Slave, Function: modbus.FuncReadHoldingRegisters, Address: off, Quantity: uint16(max(1, len(req.Values)))}
	if area == modbus.AreaCoils {
		back.Function = modbus.FuncReadCoils
	}
	resp, err := c.Do(ctx, back)
	if err != nil {
		fail("写入成功，回读失败：%v", err)
	}
	if area == modbus.AreaCoils {
		res.Readback = map[bool]string{true: "1", false: "0"}[resp.Bits[0]]
		res.Verified = resp.Bits[0] == req.Bits[0]
	} else {
		res.Readback, res.Verified = show(resp.Registers), slices.Equal(resp.Registers, req.Values)
	}
	res.OK = res.Verified
	if !res.Verified {
		res.Error = fmt.Sprintf("回读 %s 与写入的 %s 不符（控制验证 FAILED）", res.Readback, res.Value)
	}
	output(res, func() {
		if res.Verified {
			fmt.Printf("回读：%s，与目标一致\n", res.Readback)
		} else {
			fmt.Println("回读：" + res.Error)
		}
	})
	if !res.Verified {
		os.Exit(1)
	}
}

// ---- detect ----

type attempt struct {
	Mode   string `json:"mode"`
	Slave  int    `json:"slave"`
	Result string `json:"result"`
}

type detectResult struct {
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Slave       int       `json:"slave,omitempty"`
	ByException bool      `json:"by_exception,omitempty"` // 靠异常响应确认，协议同样是对的
	Attempts    []attempt `json:"attempts"`
}

func runDetect(ctx context.Context) {
	if *target == "" {
		fail("detect 需要 -target（识别 Modbus TCP 和 RTU over TCP）")
	}
	dial := func(ctx context.Context) (modbus.Transport, error) { return transport.DialTCP(ctx, *target, dialTimeout()) }
	opts := detect.Options{Timeout: *timeout, Slaves: []byte{byte(*slave)}} // 先试 -slave，再试常见的 1、255
	for _, id := range []byte{1, 255} {
		if id != byte(*slave) {
			opts.Slaves = append(opts.Slaves, id)
		}
	}
	if *trace {
		opts.Observer = modbus.ObserverFunc(printPacket)
	}
	r, err := detect.Detect(ctx, dial, opts)
	res := detectResult{OK: err == nil, Attempts: []attempt{}}
	for _, a := range r.Attempts {
		result := "有响应"
		if a.Err != nil {
			result = a.Err.Error()
		}
		res.Attempts = append(res.Attempts, attempt{modeName(a.Mode), int(a.Slave), result})
	}
	if err != nil {
		res.Error = fmt.Sprintf("%v：连接成功但两种格式都没有得到可解析的响应", err)
	} else {
		res.Mode, res.Slave, res.ByException = modeName(r.Mode), int(r.Slave), r.ByException
	}
	output(res, func() {
		for _, a := range res.Attempts {
			fmt.Printf("  %-15s Slave %-3d %s\n", a.Mode, a.Slave, a.Result)
		}
		if err == nil {
			how := "正常响应"
			if res.ByException {
				how = "异常响应（同样能确认协议）"
			}
			fmt.Printf("\n识别结果：%s，Slave %d，依据：%s\n", res.Mode, res.Slave, how)
		}
	})
	if err != nil {
		if !*jsonOut {
			fmt.Fprintln(os.Stderr, "modbus-cli: "+res.Error)
		}
		os.Exit(1)
	}
}

// ---- scan ----

type scanHit struct {
	Slave int     `json:"slave"`
	Probe string  `json:"probe"` // 哪次读取得到的应答，例如 40001（FC03）
	RTTMS float64 `json:"rtt_ms"`
	Note  string  `json:"note"`
}

type scanResult struct {
	OK      bool      `json:"ok"`
	From    int       `json:"from"`
	To      int       `json:"to"`
	Probes  []string  `json:"probes"`            // 每个站号依次试的读取
	Stopped bool      `json:"stopped,omitempty"` // 按了 Ctrl+C，只搜了一部分
	Found   []scanHit `json:"found"`
}

// runScan 搜索应答的站号，和桌面版、Web 版“搜索站号”同一套算法（internal/slavescan）：每个站号依次读
// 用户给的地址、40001、30001、线圈 1，有一次应答就算找到。异常响应也说明设备在线；网关回的异常 0A、0B 不算。
func runScan(ctx context.Context, args []string) {
	from, to := 1, 247
	if len(args) >= 2 {
		var e1, e2 error
		from, e1 = strconv.Atoi(args[0])
		to, e2 = strconv.Atoi(args[1])
		if e1 != nil || e2 != nil {
			fail("站号范围应在 1–255 之间，起始不大于结束")
		}
	}
	var extra []slavescan.Probe
	if len(args) == 3 {
		p, err := slavescan.ParseProbe(args[2])
		if err != nil {
			fail("地址 %s：%v", args[2], err)
		}
		extra = append(extra, p)
	}
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "timeout" })
	if !explicit {
		*timeout = 200 * time.Millisecond
	}
	if err := slavescan.Validate(from, to, *timeout); err != nil {
		fail("%v", err)
	}
	probes := slavescan.Probes(extra...)
	res := scanResult{OK: true, From: from, To: to, Found: []scanHit{}}
	for _, p := range probes {
		res.Probes = append(res.Probes, p.String())
	}
	c := connect(ctx, 0)
	defer c.Close()
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "每个站号依次读 %s，有一次应答就算找到；都不应答时最多要 %s，Ctrl+C 停止\n",
			strings.Join(res.Probes, "、"), eta.Rough(slavescan.Worst(from, to, *timeout, len(probes))))
	}

	// Ctrl+C 停止搜索，保留已找到的站号
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt)
	started, last, shown := time.Now(), time.Now(), 0
	hits, err := slavescan.Scan(ctx, c, from, to, *timeout, probes, func(id int, hits []slavescan.Hit) {
		if *jsonOut {
			return
		}
		for _, h := range hits[shown:] {
			fmt.Printf("Slave %-3d 应答 %.1f ms · %s\n", h.ID, millis(h.RTT), h.Note())
		}
		shown = len(hits)
		if time.Since(last) >= 2*time.Second && id < to {
			last = time.Now()
			fmt.Fprintf(os.Stderr, "已搜到 Slave %d / %d · %s\n", id, to, eta.Text(float64(id-from+1)/float64(to-from+1), started))
		}
	})
	res.Stopped = ctx.Err() != nil
	stopSignals()
	if err != nil {
		fail("搜索中途连接断开：%v", err)
	}
	for _, h := range hits {
		res.Found = append(res.Found, scanHit{Slave: h.ID, Probe: h.Probe.String(), RTTMS: millis(h.RTT), Note: h.Note()})
	}
	output(res, func() {
		switch {
		case res.Stopped:
			fmt.Println("已停止，上面是已经搜到的站号。")
		case len(res.Found) == 0:
			fmt.Printf("Slave %d–%d 都没有应答。检查接线、串口参数和协议，或用 -timeout 加大每次读取的等待时间。\n", from, to)
		}
	})
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---- raw ----

type rawResult struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Slave    uint   `json:"slave"`
	Request  string `json:"request"`  // 请求 PDU（功能码 + 数据），十六进制
	Response string `json:"response"` // 响应 PDU；异常响应也有
}

// runRaw 发送任意 PDU（功能码 + 数据），打印响应 PDU，用于调试 FC08、FC2B 等不常用功能码。
func runRaw(ctx context.Context, text string) {
	pdu, err := hex.DecodeString(strings.NewReplacer(" ", "", "-", "", ":", "").Replace(text))
	if err != nil || len(pdu) == 0 {
		fail("raw 后面写请求 PDU 的十六进制（功能码 + 数据），例如 raw 03 00 00 00 0A")
	}
	c := connect(ctx, 0)
	defer c.Close()
	resp, err := c.DoRaw(ctx, byte(*slave), pdu)
	res := rawResult{OK: err == nil, Slave: *slave, Request: hexs(pdu), Response: hexs(resp)}
	if err != nil && resp == nil {
		explain(modbus.Request{Slave: byte(*slave), Function: modbus.FunctionCode(pdu[0])}, err)
	}
	if err != nil {
		res.Error = err.Error()
	}
	output(res, func() {
		fmt.Printf("请求 PDU  %s\n响应 PDU  %s\n", res.Request, res.Response)
		if res.Error != "" {
			fmt.Println("异常：" + res.Error)
		}
	})
	if err != nil {
		os.Exit(1)
	}
}
