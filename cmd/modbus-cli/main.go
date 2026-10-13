// modbus-cli 是命令行 Modbus 主站：读写寄存器和线圈、识别协议、扫描从站、发自定义请求。
// 加 -json 时每条命令在标准输出打印一个 JSON 对象，方便脚本和 AI 调用；modbus-cli manual 打印完整说明。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
)

// version 在打包时用 -ldflags "-X main.version=…" 覆盖。
var version = "dev"

var (
	target  = flag.String("target", "", "TCP 地址，例如 192.168.1.100:502")
	port    = flag.String("port", "", "串口，例如 COM3、/dev/ttyUSB0、/dev/cu.usbserial-xxx")
	baud    = flag.Int("baud", 9600, "波特率")
	parity  = flag.String("parity", "N", "校验位：N、E、O")
	stop    = flag.Int("stop", 1, "停止位：1 或 2")
	modeArg = flag.String("mode", "", "协议：tcp、rtu-over-tcp、ascii-over-tcp、rtu、ascii（默认 TCP 地址用 tcp，串口用 rtu）")
	dataBit = flag.Int("databits", 8, "数据位：8，Modbus ASCII 常用 7")
	slave   = flag.Uint("slave", 1, "Slave ID（站号）")
	timeout = flag.Duration("timeout", time.Second, "响应超时，例如 500ms、3s；scan 没指定时每个站号等 200ms")
	fcArg   = flag.Int("fc", 0, "功能码。read：1 线圈、2 离散输入、3 保持寄存器、4 输入寄存器；\nwrite：5 单个线圈、6 单个寄存器、16 多个寄存器。不填时按地址写法判断（40001 → 3，30001 → 4，10001 → 2）")
	typ     = flag.String("type", "uint16", "寄存器的数据类型：int16、uint16、int32、uint32、float32、int64、uint64、float64")
	order   = flag.String("order", "", "字节序：AB、BA、ABCD、CDAB、BADC、DCBA，64 位还可写 ABCDEFGH、GHEFCDAB、BADCFEHG、HGFEDCBA\n（默认标准大端；64 位写 32 位的名字时按同样规则换算，CDAB 即 GHEFCDAB）")
	scale   = flag.Float64("scale", 1, "工程值 = 原始值 × scale")
	jsonOut = flag.Bool("json", false, "结果按 JSON 打印到标准输出（一个对象，ok 表示是否成功），方便脚本和 AI 解析")
	trace   = flag.Bool("trace", false, "把收发报文打印到标准错误")
	echo    = flag.Bool("echo", false, "丢弃 RS485 转换器回显的发送帧")
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
	default:
		if *jsonOut {
			output(failure{Error: "命令或参数个数不对，运行 modbus-cli manual 查看用法"}, nil)
		} else {
			flag.Usage()
		}
		os.Exit(2)
	}
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

// connect 按选项连接。retries 是读请求超时后的重试次数，扫描从站时不重试。
func connect(ctx context.Context, retries int) *modbus.Client {
	m := mode()
	opts := modbus.DefaultOptions(m)
	opts.Timeout, opts.DiscardEcho, opts.ReadRetries = *timeout, *echo, retries
	if *trace {
		opts.Observer = modbus.ObserverFunc(printPacket)
	}
	var t modbus.Transport
	var err error
	switch {
	case *port != "":
		t, err = transport.OpenSerial(transport.SerialConfig{Port: *port, BaudRate: *baud, DataBits: *dataBit, Parity: *parity, StopBits: *stop})
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

// explain 把通信错误换成带建议的说明后报错。
func explain(err error) {
	if ex, ok := modbus.AsException(err); ok {
		fail("%v。建议：%s", ex, ex.Code.Tip())
	}
	if errors.Is(err, modbus.ErrTimeout) {
		fail("%v：%v 内没有匹配的响应。检查 Slave ID、协议类型和接线；加 -trace 查看报文", err, *timeout)
	}
	fail("%v", err)
}
