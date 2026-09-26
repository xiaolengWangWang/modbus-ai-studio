// modbus-cli 是最小的命令行主站，用来在 GUI 完成前联调协议核心。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/detect"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
)

const usage = `用法：
  modbus-cli [选项] read  <地址> <数量>   读寄存器，地址支持 346、0x015A、40347、4x0347
  modbus-cli [选项] write <地址> <值>     按 -type/-order 编码写入，并立即回读验证
  modbus-cli [选项] detect               自动识别 Modbus TCP / RTU over TCP

示例：
  modbus-cli -target 127.0.0.1:1502 -type float32 -order CDAB read 40001 4
  modbus-cli -target 127.0.0.1:1502 -type float32 -order CDAB write 40347 16
  modbus-cli -port /dev/cu.usbserial-110 -baud 9600 read 40001 10

选项：`

var (
	target  = flag.String("target", "", "TCP 地址，例如 192.168.1.100:502")
	port    = flag.String("port", "", "串口，例如 COM3、/dev/ttyUSB0、/dev/cu.usbserial-xxx")
	baud    = flag.Int("baud", 9600, "波特率")
	parity  = flag.String("parity", "N", "校验位：N、E、O")
	stop    = flag.Int("stop", 1, "停止位：1 或 2")
	modeArg = flag.String("mode", "", "协议：tcp、rtu-over-tcp、rtu（默认 TCP 地址用 tcp，串口用 rtu）")
	slave   = flag.Uint("slave", 1, "Slave ID")
	timeout = flag.Duration("timeout", time.Second, "响应超时")
	typ     = flag.String("type", "uint16", "数据类型：int16、uint16、int32、uint32、float32")
	order   = flag.String("order", "", "字节序：AB、BA、ABCD、CDAB、BADC、DCBA（默认 16 位 AB，32 位 ABCD）")
	scale   = flag.Float64("scale", 1, "工程值 = 原始值 × scale")
	trace   = flag.Bool("trace", false, "打印收发报文")
	echo    = flag.Bool("echo", false, "丢弃 RS485 回显")
)

func main() {
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	if args[0] == "detect" {
		runDetect(ctx)
		return
	}
	if len(args) != 3 || (args[0] != "read" && args[0] != "write") {
		flag.Usage()
		os.Exit(2)
	}
	dt, bo := dataType(), byteOrder()
	cand := resolveAddress(args[1])
	c := connect(ctx)
	defer c.Close()
	if args[0] == "read" {
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 {
			fail("数量应为正整数")
		}
		runRead(ctx, c, cand, n, dt, bo)
		return
	}
	v, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		fail("值应为数字")
	}
	runWrite(ctx, c, cand, v, dt, bo)
}

func mode() modbus.Mode {
	switch *modeArg {
	case "tcp":
		return modbus.ModeTCP
	case "rtu-over-tcp":
		return modbus.ModeRTUOverTCP
	case "rtu":
		return modbus.ModeRTU
	case "":
		if *port != "" {
			return modbus.ModeRTU
		}
		return modbus.ModeTCP
	}
	fail("不支持的协议 %q", *modeArg)
	return ""
}

func connect(ctx context.Context) *modbus.Client {
	m := mode()
	opts := modbus.DefaultOptions(m)
	opts.Timeout, opts.DiscardEcho = *timeout, *echo
	if *trace {
		opts.Observer = modbus.ObserverFunc(printPacket)
	}
	var t modbus.Transport
	var err error
	switch {
	case *port != "":
		t, err = transport.OpenSerial(transport.SerialConfig{Port: *port, BaudRate: *baud, Parity: *parity, StopBits: *stop})
	case *target != "":
		t, err = transport.DialTCP(ctx, *target, *timeout)
	default:
		fail("需要 -target 或 -port")
	}
	if err != nil {
		fail("连接失败：%v", err)
	}
	return modbus.NewClient(t, opts)
}

// 与 Modbus Poll 通信报文窗口相同的格式：Tx:000001-01 03 …
func printPacket(p modbus.Packet) {
	tag := "Rx"
	if p.Dir == modbus.DirTX {
		tag = "Tx"
	}
	line := fmt.Sprintf("%s:%06d-%s", tag, p.RequestID, hexs(p.Raw))
	if p.Raw == nil {
		line = fmt.Sprintf("%s:%06d-（无数据）", tag, p.RequestID)
	}
	if p.Status != modbus.StatusSent && p.Status != modbus.StatusSuccess {
		line += "  ← " + string(p.Status)
	}
	fmt.Fprintln(os.Stderr, line)
}

func hexs(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

func dataType() modbus.DataType {
	t := modbus.DataType(strings.ToUpper(*typ))
	switch t {
	case modbus.TypeInt16, modbus.TypeUint16, modbus.TypeInt32, modbus.TypeUint32, modbus.TypeFloat32:
		return t
	}
	fail("不支持的数据类型 %q", *typ)
	return ""
}

func byteOrder() modbus.ByteOrder {
	if *order != "" {
		return modbus.ByteOrder(strings.ToUpper(*order))
	}
	if dataType().Registers() == 2 {
		return modbus.OrderABCD
	}
	return modbus.OrderAB
}

// 地址有歧义时按第一种解释执行，并明确告诉用户另一种写法（设计文档 6.3：不静默猜测）。
func resolveAddress(s string) modbus.AddressCandidate {
	cands, err := modbus.ParseAddress(s)
	if err != nil {
		fail("%v", err)
	}
	if len(cands) > 1 {
		fmt.Fprintf(os.Stderr, "注意：%s 有 %d 种解释，已按“%s → Offset %d”执行；", s, len(cands), cands[0].Label, cands[0].Offset)
		fmt.Fprintf(os.Stderr, "如果指的是原始 Offset %d，请写成 0x%04X。\n", cands[1].Offset, cands[1].Offset)
	}
	return cands[0]
}

func runRead(ctx context.Context, c *modbus.Client, a modbus.AddressCandidate, n int, dt modbus.DataType, bo modbus.ByteOrder) {
	fc := a.Area.ReadFunction()
	if fc != modbus.FuncReadHoldingRegisters && fc != modbus.FuncReadInputRegisters {
		fail("modbus-cli 目前只读寄存器（3x、4x）")
	}
	start := time.Now()
	resp, err := c.Do(ctx, modbus.Request{Slave: byte(*slave), Function: fc, Address: a.Offset, Quantity: uint16(n)})
	if err != nil {
		explain(err)
	}
	area := modbus.AreaOf(fc)
	fmt.Printf("%s · Slave %d · %s × %d · %v\n\n", c.Mode(), *slave, modbus.Reference(area, a.Offset), n, time.Since(start).Round(time.Millisecond))
	w := dt.Registers()
	fmt.Printf("%-8s %-7s %-12s %s\n", "地址", "Offset", "寄存器", fmt.Sprintf("%s %s", dt, bo))
	for i := 0; i+w <= len(resp.Registers); i += w {
		regs := resp.Registers[i : i+w]
		hexRegs := make([]string, w)
		for j, r := range regs {
			hexRegs[j] = fmt.Sprintf("%04X", r)
		}
		val := "—"
		if raw, err := modbus.DecodeRaw(dt, bo, regs); err == nil {
			val = formatEng(modbus.Scaling{Scale: *scale}.Engineering(raw), dt)
			if dt == modbus.TypeFloat32 {
				if sug, ok := modbus.SuggestByteOrder(bo, regs); ok {
					val += fmt.Sprintf("（不合理，按 %s 解码更可能正确）", sug)
				}
			}
		} else {
			fail("%v", err)
		}
		off := a.Offset + uint16(i)
		fmt.Printf("%-8s %-7d %-12s %s\n", modbus.Reference(area, off), off, strings.Join(hexRegs, " "), val)
	}
}

// formatEng 格式化工程值：FLOAT32 按单精度显示（45.2 而不是 45.200001），
// 整型按 Scale 的小数位数显示（132 × 0.1 显示 13.2）。
func formatEng(v float64, dt modbus.DataType) string {
	if dt == modbus.TypeFloat32 && *scale == 1 {
		return strconv.FormatFloat(v, 'g', -1, 32)
	}
	s := strconv.FormatFloat(*scale, 'f', -1, 64)
	decimals := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimals = len(s) - i - 1
	}
	if dt == modbus.TypeFloat32 {
		decimals += 3
	}
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// runWrite 先展示编码，再写入，并立即回读比较（控制验证的最小形态，完整流程见设计文档 11.4）。
func runWrite(ctx context.Context, c *modbus.Client, a modbus.AddressCandidate, eng float64, dt modbus.DataType, bo modbus.ByteOrder) {
	if !a.Area.Writable() {
		fail("%s 区只读，不能写入", a.Area.Prefix())
	}
	raw, rounded := modbus.Scaling{Scale: *scale}.Raw(dt, eng)
	regs, err := modbus.EncodeRaw(dt, bo, raw)
	if err != nil {
		fail("%v", err)
	}
	req := modbus.Request{Slave: byte(*slave), Function: modbus.FuncWriteSingleRegister, Address: a.Offset, Values: regs}
	if len(regs) > 1 {
		req.Function = modbus.FuncWriteMultipleRegisters
	}
	pdu, _ := req.PDU()
	fmt.Printf("目标值   %v（%s %s）\n", eng, dt, bo)
	if rounded {
		fmt.Printf("注意     取整后实际写入原始值 %v\n", raw)
	}
	fmt.Printf("线上字节 %s\n寄存器   ", hexs(modbus.RegistersToBytes(regs)))
	for i, r := range regs {
		fmt.Printf("%s = 0x%04X  ", modbus.Reference(modbus.AreaHoldingRegisters, a.Offset+uint16(i)), r)
	}
	fmt.Printf("\n请求     %s\n\n", hexs(modbus.EncodeADU(c.Mode(), req.Slave, 1, pdu)))

	if _, err := c.Do(ctx, req); err != nil {
		explain(err)
	}
	fmt.Println("协议写入：SUCCESS")
	resp, err := c.Do(ctx, modbus.Request{Slave: req.Slave, Function: modbus.FuncReadHoldingRegisters, Address: a.Offset, Quantity: uint16(len(regs))})
	if err != nil {
		fmt.Print("回读失败：")
		explain(err)
	}
	back, _ := modbus.DecodeRaw(dt, bo, resp.Registers)
	backEng := modbus.Scaling{Scale: *scale}.Engineering(back)
	if backEng == eng || (dt == modbus.TypeFloat32 && float32(backEng) == float32(eng)) {
		fmt.Printf("回读：%v，与目标一致\n", backEng)
		return
	}
	fmt.Printf("回读：%v，与目标 %v 不符（控制验证 FAILED）\n", backEng, eng)
	os.Exit(1)
}

func runDetect(ctx context.Context) {
	if *target == "" {
		fail("detect 需要 -target")
	}
	dial := func(ctx context.Context) (modbus.Transport, error) { return transport.DialTCP(ctx, *target, *timeout) }
	opts := detect.Options{Timeout: *timeout}
	if *trace {
		opts.Observer = modbus.ObserverFunc(printPacket)
	}
	res, err := detect.Detect(ctx, dial, opts)
	for _, at := range res.Attempts {
		outcome := "有响应"
		if at.Err != nil {
			outcome = at.Err.Error()
		}
		fmt.Printf("  %-13s Slave %-3d %s\n", at.Mode, at.Slave, outcome)
	}
	if err != nil {
		fail("%v：连接成功但两种格式都没有得到可解析的响应", err)
	}
	how := "正常响应"
	if res.ByException {
		how = "异常响应（同样能确认协议）"
	}
	fmt.Printf("\n识别结果：%s，Slave %d，依据：%s\n", res.Mode, res.Slave, how)
}

func explain(err error) {
	if ex, ok := modbus.AsException(err); ok {
		fail("%v。建议：%s", ex, ex.Code.Tip())
	}
	if errors.Is(err, modbus.ErrTimeout) {
		fail("%v：%v 内没有匹配的响应。检查 Slave ID、协议类型和接线；加 -trace 查看报文", err, *timeout)
	}
	fail("%v", err)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "modbus-cli: "+format+"\n", a...)
	os.Exit(1)
}
