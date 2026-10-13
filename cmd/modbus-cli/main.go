// modbus-cli 是命令行 Modbus 主站：读写寄存器和线圈、识别协议、扫描从站、发自定义请求。
// 加 -json 时每条命令在标准输出打印一个 JSON 对象，方便脚本和 AI 调用；modbus-cli manual 打印完整说明。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"modbus-ai-studio/internal/diagnose"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
)

// version 在打包时用 -ldflags "-X main.version=…" 覆盖。
var version = "dev"

var (
	target     = flag.String("target", "", "TCP 地址，例如 192.168.1.100:502")
	port       = flag.String("port", "", "串口，例如 COM3、/dev/ttyUSB0、/dev/cu.usbserial-xxx")
	baud       = flag.Int("baud", 9600, "波特率")
	parity     = flag.String("parity", "N", "校验位：N、E、O")
	stop       = flag.Int("stop", 1, "停止位：1 或 2")
	modeArg    = flag.String("mode", "", "协议：tcp、rtu-over-tcp、ascii-over-tcp、rtu、ascii（默认 TCP 地址用 tcp，串口用 rtu）")
	dataBit    = flag.Int("databits", 8, "数据位：8，Modbus ASCII 常用 7")
	slave      = flag.Uint("slave", 1, "Slave ID（站号）")
	timeout    = flag.Duration("timeout", time.Second, "响应超时，例如 500ms、3s；scan 没指定时每个站号等 200ms")
	fcArg      = flag.Int("fc", 0, "功能码。read：1 线圈、2 离散输入、3 保持寄存器、4 输入寄存器；\nwrite：5 单个线圈、6 单个寄存器、16 多个寄存器。不填时按地址写法判断（40001 → 3，30001 → 4，10001 → 2）")
	typ        = flag.String("type", "uint16", "寄存器的数据类型：int16、uint16、int32、uint32、float32、int64、uint64、float64")
	order      = flag.String("order", "", "字节序：AB、BA、ABCD、CDAB、BADC、DCBA，64 位还可写 ABCDEFGH、GHEFCDAB、BADCFEHG、HGFEDCBA\n（默认标准大端；64 位写 32 位的名字时按同样规则换算，CDAB 即 GHEFCDAB）")
	scale      = flag.Float64("scale", 1, "工程值 = 原始值 × scale")
	pointsFile = flag.String("points", "", "点表文件（CSV 或 xlsx，本程序的点表或平台导出的设备属性表），points、get、set 命令用")
	reportFile = flag.String("report", "", "probe 检测完把报告写到这个 .md 文件，同目录另存同名的 JSONL 明细和 CSV 失败清单")
	reportNote = flag.String("report-note", "", "写进检测报告的实际设备备注（例如经过 DTU 转发时设备真正的地址），只是备注，不用于连接")
	jsonOut    = flag.Bool("json", false, "结果按 JSON 打印到标准输出（一个对象，ok 表示是否成功），方便脚本和 AI 解析")
	trace      = flag.Bool("trace", false, "把收发报文打印到标准错误")
	echo       = flag.Bool("echo", false, "丢弃 RS485 转换器回显的发送帧")
)

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch cmd, rest := args[0], args[1:]; {
	case cmd == "version" && len(rest) == 0:
		output(map[string]any{"ok": true, "version": version}, func() { fmt.Println("modbus-cli", version) })
	case cmd == "manual" && len(rest) == 0:
		fmt.Print(manual())
	case cmd == "read" && len(rest) == 2:
		runRead(ctx, rest[0], rest[1])
	case cmd == "write" && len(rest) == 2:
		runWrite(ctx, rest[0], rest[1])
	case cmd == "detect" && len(rest) == 0:
		runDetect(ctx)
	case cmd == "scan" && (len(rest) == 0 || len(rest) == 2):
		runScan(ctx, rest)
	case cmd == "raw" && len(rest) > 0:
		runRaw(ctx, strings.Join(rest, ""))
	case cmd == "points" && len(rest) == 0:
		runPoints()
	case cmd == "get":
		runGet(ctx, rest)
	case cmd == "set" && len(rest) == 2:
		runSet(ctx, rest[0], rest[1])
	case cmd == "probe":
		runProbe(ctx, rest)
	default:
		badUsage()
	}
}

// badUsage 报命令或参数个数写错，以退出码 2 结束。
func badUsage() {
	if *jsonOut {
		output(failure{Error: "命令或参数个数不对，运行 modbus-cli manual 查看用法"}, nil)
	} else {
		flag.Usage()
	}
	os.Exit(2)
}

// modes 是 -mode 的写法，JSON 输出里的 mode 也用它。
var modes = map[string]modbus.Mode{"tcp": modbus.ModeTCP, "rtu-over-tcp": modbus.ModeRTUOverTCP, "ascii-over-tcp": modbus.ModeASCIIOverTCP,
	"rtu": modbus.ModeRTU, "ascii": modbus.ModeASCII}

func mode() modbus.Mode {
	if m, ok := modes[*modeArg]; ok {
		return m
	}
	if *modeArg != "" {
		fail("不支持的协议 %q", *modeArg)
	}
	if *port != "" {
		return modbus.ModeRTU
	}
	return modbus.ModeTCP
}

func modeName(m modbus.Mode) string {
	for name, x := range modes {
		if x == m {
			return name
		}
	}
	return string(m)
}

// connect 按选项连接。retries 是读请求超时后的重试次数，扫描从站、检测寄存器时不重试；observe 另外接收收发的报文。
func connect(ctx context.Context, retries int, observe ...func(modbus.Packet)) *modbus.Client {
	m := mode()
	opts := modbus.DefaultOptions(m)
	opts.Timeout, opts.DiscardEcho, opts.ReadRetries = *timeout, *echo, retries
	opts.Observer = observer(observe...)
	var t modbus.Transport
	var err error
	switch {
	case *port != "":
		t, err = transport.OpenSerial(transport.SerialConfig{Port: *port, BaudRate: *baud, DataBits: *dataBit, Parity: *parity, StopBits: *stop})
	case *target != "":
		t, err = transport.DialTCP(ctx, *target, dialTimeout())
	default:
		fail("需要 -target 或 -port")
	}
	if err != nil {
		fail("连接失败：%v", err)
	}
	return modbus.NewClient(t, opts)
}

// dialTimeout 是建 TCP 连接的超时：至少 3 秒。-timeout 是等响应的时间，scan 默认只有 200 ms，
// 拿来建连接太短，设备或网络慢一点就连不上。
func dialTimeout() time.Duration { return max(*timeout, 3*time.Second) }

// observer 把收发的报文交给 observe，加 -trace 时还打印到标准错误；都没有时返回 nil。
func observer(observe ...func(modbus.Packet)) modbus.Observer {
	if *trace {
		observe = append(observe, printPacket)
	}
	if len(observe) == 0 {
		return nil
	}
	return modbus.ObserverFunc(func(p modbus.Packet) {
		for _, f := range observe {
			f(p)
		}
	})
}

// printPacket 用和 Modbus Poll 通信报文窗口相同的格式打印到标准错误：Tx:000001-01 03 …
func printPacket(p modbus.Packet) {
	tag := "Rx"
	if p.Dir == modbus.DirTX {
		tag = "Tx"
	}
	data := hexs(p.Raw)
	if p.Mode.IsASCII() {
		data = strings.TrimSuffix(string(p.Raw), "\r\n") // ASCII 帧本身就是可读字符
	}
	line := fmt.Sprintf("%s:%06d-%s", tag, p.RequestID, data)
	if p.Raw == nil {
		line = fmt.Sprintf("%s:%06d-（无数据）", tag, p.RequestID)
	}
	if p.Status != modbus.StatusSent && p.Status != modbus.StatusSuccess {
		line += "  ← " + string(p.Status)
		if p.Err != nil {
			line += "：" + p.Err.Error()
		}
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

// failure 是 -json 时出错的输出。
type failure struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// output 打印结果：-json 时打印 v 的 JSON，否则调用 text 打印给人看的文字。
func output(v any, text func()) {
	if !*jsonOut {
		text()
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "modbus-cli:", err)
		os.Exit(1)
	}
}

// fail 报错并以退出码 1 结束：-json 时在标准输出打印 {"ok": false, "error": …}。
func fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if *jsonOut {
		output(failure{Error: msg}, nil)
	} else {
		fmt.Fprintln(os.Stderr, "modbus-cli: "+msg)
	}
	os.Exit(1)
}

// explain 说明通信错误后报错，见 describe。
func explain(req modbus.Request, err error) { fail("%s", describe(req, err)) }

// describe 用和桌面版、Web 版同一套规则（internal/diagnose）说明通信错误：出了什么错、可能的原因、建议怎么做。
func describe(req modbus.Request, err error) string {
	r := diagnose.Analyze(diagnose.Request{Slave: req.Slave, Function: req.Function, Start: req.Address, Qty: req.Count()},
		diagnose.Context{Mode: mode(), Timeout: *timeout}, err)
	text := r.Text
	if r.Hint != "" {
		text += "。" + strings.TrimSuffix(r.Hint, "。")
	}
	if a := cliAdvice(r); a != "" {
		text += "。建议：" + a
	}
	return text
}

// cliAdvice 把一键处理换成命令行上的做法。
func cliAdvice(r diagnose.Result) string {
	switch r.Action {
	case diagnose.SetTimeout:
		return fmt.Sprintf("加 -timeout %dms 重试", r.Timeout.Milliseconds())
	case diagnose.UseFC03:
		return "改用 -fc 3 读保持寄存器"
	case diagnose.UseFC04:
		return "改用 -fc 4 读输入寄存器"
	case diagnose.HalveQty:
		return "把数量减半"
	case diagnose.ProbeRange:
		return "缩小范围逐段读，找出哪些地址读不通"
	case diagnose.ScanSlaves:
		return "运行 scan 找出在应答的站号"
	case diagnose.DetectProtocol:
		return "运行 detect 识别协议，或加 -trace 查看收发报文"
	case diagnose.ScanSerial:
		return "核对 -baud、-parity、-stop 和站号，加 -trace 查看收发报文"
	}
	return ""
}
