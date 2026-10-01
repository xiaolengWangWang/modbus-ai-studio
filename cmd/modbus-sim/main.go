// modbus-sim 是命令行 Slave 模拟器，默认加载换热站示例点表，供联调和 CI 使用。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:1502", "监听地址")
	modeName := flag.String("mode", "tcp", "协议：tcp、rtu-over-tcp 或 ascii-over-tcp")
	slave := flag.Uint("slave", 1, "Slave ID，0 表示响应任意 ID")
	delay := flag.Duration("delay", 0, "固定响应延迟，例如 20ms")
	drop := flag.Float64("drop", 0, "不响应的比例，0–1")
	crcRate := flag.Float64("crc", 0, "RTU over TCP 响应 CRC 错误的比例，0–1")
	slow := flag.String("slow", "", "慢应答地址段：起始:数量:延迟，多段用逗号分隔，例如 600:4:1035ms")
	static := flag.Bool("static", false, "数据保持初值（默认流量、热功率、累计量每秒更新）")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法：modbus-sim [选项]\n\n换热站 PLC 模拟从站，点表见设计文档 13.5。\n\n选项：")
		flag.PrintDefaults()
	}
	flag.Parse()

	var mode modbus.Mode
	switch *modeName {
	case "tcp":
		mode = modbus.ModeTCP
	case "rtu-over-tcp":
		mode = modbus.ModeRTUOverTCP
	case "ascii-over-tcp":
		mode = modbus.ModeASCIIOverTCP
	default:
		fail("不支持的协议 %q，可选 tcp、rtu-over-tcp、ascii-over-tcp", *modeName)
	}
	if *slave > 247 {
		fail("Slave ID 应为 0–247")
	}
	slowRanges, err := parseSlow(*slow)
	if err != nil {
		fail("%v", err)
	}

	store := simulator.HeatStation()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !*static {
		go simulator.RunHeatStation(ctx, store)
	}
	srv := simulator.NewServer(mode, byte(*slave), store)
	srv.SetFaults(simulator.Faults{Delay: *delay, DropRate: *drop, CRCRate: *crcRate, Slow: slowRanges})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fail("监听 %s 失败：%v", *listen, err)
	}
	fmt.Printf("换热站模拟器：%s · %s · Slave %d（Ctrl+C 退出）\n\n", ln.Addr(), mode, *slave)
	printPoints()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil {
		fail("%v", err)
	}
}

func parseSlow(s string) ([]simulator.SlowRange, error) {
	var out []simulator.SlowRange
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		f := strings.Split(part, ":")
		if len(f) != 3 {
			return nil, fmt.Errorf("慢应答格式应为 起始:数量:延迟，得到 %q", part)
		}
		start, err1 := strconv.ParseUint(f[0], 10, 16)
		count, err2 := strconv.ParseUint(f[1], 10, 16)
		d, err3 := time.ParseDuration(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("慢应答 %q 无法解析", part)
		}
		out = append(out, simulator.SlowRange{AddrRange: simulator.AddrRange{Start: uint16(start), Count: uint16(count)}, Delay: d})
	}
	return out, nil
}

func printPoints() {
	fmt.Printf("%-7s %-12s %-8s %-5s %-6s %-5s %s\n", "地址", "名称", "类型", "字节序", "Scale", "单位", "读写")
	for _, p := range simulator.HeatStationPoints {
		rw := "R"
		if p.RW {
			rw = "RW"
		}
		fmt.Printf("%-7s %-12s %-8s %-5s %-6v %-5s %s\n", modbus.Reference(modbus.AreaHoldingRegisters, p.Offset), p.Name, p.Type, p.Order, p.Scale, p.Unit, rw)
	}
	fmt.Println()
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "modbus-sim: "+format+"\n", a...)
	os.Exit(1)
}
