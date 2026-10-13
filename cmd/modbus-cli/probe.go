package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/eta"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
	"modbus-ai-studio/internal/probe"
	"modbus-ai-studio/internal/transport"
)

// ---- probe ----

// 检测寄存器和桌面版用同一套算法（internal/probe）：找出一段地址或点表里哪些读得通、哪些是非法地址、
// 哪些无法判断。按 Ctrl+C 停止，保留已完成的结果。

// probeModes 是 probe 后面的检测方式，JSON 输出里的 mode 也用它。
var probeModes = map[string]probe.Mode{"auto": probe.ModeAuto, "single": probe.ModeSingle, "points": probe.ModePoints,
	"each": probe.ModeEachPoint, "quick": probe.ModeQuick}

type probeSpan struct {
	Start  string `json:"start"`
	Offset int    `json:"offset"`
	Count  int    `json:"count"`
}

type probeResult struct {
	OK        bool                `json:"ok"`
	Error     string              `json:"error,omitempty"` // 中途停止或报告没写成的原因；已完成的结果照样给出
	Mode      string              `json:"mode"`
	Slave     int                 `json:"slave"`
	Function  int                 `json:"function"`
	Start     string              `json:"start"`
	Offset    int                 `json:"offset"`
	Count     int                 `json:"count"`
	Points    int                 `json:"points,omitempty"` // 按点表检测时用到的点数
	Requests  int                 `json:"requests"`
	ElapsedMS int64               `json:"elapsed_ms"`
	Summary   string              `json:"summary"`
	Problems  bool                `json:"problems"`           // 有读不通、非法或无法判断的地址（点）
	Readable  *probeSpan          `json:"readable,omitempty"` // 最长的连续可读范围，常规读取可以改用（auto、single）
	Failures  []map[string]string `json:"failures"`           // 失败清单，字段和导出的 CSV 一样
	Text      string              `json:"text"`               // 完整的结果说明，和桌面版结果框一样
	Report    []string            `json:"report,omitempty"`   // -report 写出的文件
}

// runProbe 检测寄存器：probe [auto|single|quick] <起始地址> <数量>，或 probe points|each（按 -points 点表）。
func runProbe(ctx context.Context, args []string) {
	name := "auto"
	if len(args) > 0 {
		if _, ok := probeModes[args[0]]; ok {
			name, args = args[0], args[1:]
		}
	}
	how := probeModes[name]
	if *slave > 255 {
		fail("Slave ID 应为 1–255")
	}
	var pts points.Table
	if *pointsFile != "" || how.UsesPoints() {
		_, pts = loadPoints()
	}
	var d probe.Def
	var segs []probe.Segment
	if how.UsesPoints() {
		if len(args) != 0 {
			badUsage()
		}
		d, segs = pointProbeDef(pts)
	} else {
		if len(args) != 2 {
			badUsage()
		}
		off, area := address(args[0], 1, 2, 3, 4)
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			fail("数量应为正整数")
		}
		d = probe.Def{Slave: byte(*slave), Function: area.ReadFunction(), Start: off, Qty: n}
	}
	if err := probe.Validate(d); err != nil {
		fail("%v", err)
	}

	m := mode()
	run := &probe.Run{Mode: how, Def: d, Target: endpoint(m), Timeout: *timeout, Log: probe.NewLog()}
	for _, s := range segs {
		run.Points = append(run.Points, s.Points...)
	}
	c, closeClient := probeClient(ctx, m, run.Log)
	defer closeClient()

	// Ctrl+C 停止检测，保留已完成的结果；结果出来以后再按就直接退出
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt)
	total, unit := probeTotal(how, d, segs)
	run.Started = time.Now()
	last := run.Started
	progress := func(n int) {
		if *jsonOut || time.Since(last) < 2*time.Second || ctx.Err() != nil {
			return
		}
		last = time.Now()
		fmt.Fprintf(os.Stderr, unit+" · %s\n", n, total, eta.Text(float64(n)/float64(total), run.Started))
	}
	client := probe.Logged{Client: c, Log: run.Log}
	var res []int8
	var checks []probe.PointCheck
	var quick *probe.QuickResult
	var stopErr error
	switch how {
	case probe.ModeAuto:
		res, stopErr = probe.Locate(ctx, client, d, progress, func(offset int) {
			if !*jsonOut && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "初检完成，正在复核未响应地址 · %s\n", modbus.Reference(modbus.AreaOf(d.Function), d.Start+uint16(offset)))
			}
		})
	case probe.ModeEachPoint:
		checks, stopErr = probe.CheckEachPoint(ctx, client, d, segs, progress)
	case probe.ModeQuick:
		quick, stopErr = probe.QuickCheck(ctx, client, d, pts, probe.QuickBudget, progress)
	default:
		res, stopErr = probe.Units(ctx, client, d, how, segs, progress)
	}
	if ctx.Err() != nil && stopErr == nil {
		stopErr = context.Canceled
	}
	stopSignals()
	closeClient()
	run.Finished, run.StopErr = time.Now(), stopErr
	if quick != nil {
		for _, u := range quick.Units {
			if u.P != nil {
				run.Points = append(run.Points, *u.P)
			}
		}
	}

	var out probe.Output
	bestStart, bestN := 0, 0
	switch how {
	case probe.ModeEachPoint:
		out = probe.EachPointOutput(d, checks, segs, stopErr)
	case probe.ModeQuick:
		out = probe.QuickText(d, quick, stopErr)
		out.Text += "\n\n" + probe.IllegalNote
	default:
		out, bestStart, bestN = probe.OutputFor(d, res, stopErr, segs...)
	}
	area := modbus.AreaOf(d.Function)
	r := probeResult{OK: stopErr == nil, Mode: name, Slave: int(d.Slave), Function: int(d.Function), Start: modbus.Reference(area, d.Start),
		Offset: int(d.Start), Count: d.Qty, Points: len(run.Points), Requests: len(run.Log.Snapshot()),
		ElapsedMS: run.Finished.Sub(run.Started).Milliseconds(), Summary: out.Summary, Problems: out.Warn, Failures: []map[string]string{}, Text: out.Text}
	if stopErr != nil {
		r.Error = "检测没有做完：" + stopErr.Error()
	}
	if bestN > 0 && (how == probe.ModeAuto || how == probe.ModeSingle) {
		r.Readable = &probeSpan{Start: modbus.Reference(area, d.Start+uint16(bestStart)), Offset: int(d.Start) + bestStart, Count: bestN}
	}
	for _, row := range out.CSVRows {
		f := map[string]string{}
		for i, h := range out.CSVHeader {
			if i < len(row) {
				f[h] = row[i]
			}
		}
		r.Failures = append(r.Failures, f)
	}
	if *reportFile != "" {
		files, err := probe.ExportReport(context.Background(), run, out, *reportNote, *reportFile)
		if err != nil {
			r.OK, r.Error = false, strings.TrimPrefix(r.Error+"；", "；")+"检测报告没有完整保存："+err.Error()
		}
		r.Report = files
	}
	output(r, func() {
		fmt.Printf("%s · %d 次请求 · %s\n\n%s\n", r.Summary, r.Requests, eta.Rough(run.Finished.Sub(run.Started)), r.Text)
		if r.Readable != nil && r.Readable.Count < d.Qty {
			fmt.Printf("\n常规读取可以改用：%s 起 %d 个（read %s %d）\n", r.Readable.Start, r.Readable.Count, r.Readable.Start, r.Readable.Count)
		}
		if len(r.Report) > 0 {
			fmt.Printf("\n检测报告：%s\n", strings.Join(r.Report, "、"))
		}
	})
	if !r.OK {
		if !*jsonOut {
			fmt.Fprintln(os.Stderr, "modbus-cli: "+r.Error)
		}
		os.Exit(1)
	}
}

// pointProbeDef 是按点表检测的范围：-fc 指定的数据区，没指定时依次找 03、04、01、02 里有点的第一个。
func pointProbeDef(pts points.Table) (probe.Def, []probe.Segment) {
	fcs := []int{3, 4, 1, 2}
	if *fcArg != 0 {
		if *fcArg < 1 || *fcArg > 4 {
			fail("probe 的 -fc 只能是 1、2、3、4")
		}
		fcs = []int{*fcArg}
	}
	for _, fc := range fcs {
		if d, segs, ok := probe.PointDef(pts, probe.Def{Slave: byte(*slave), Function: modbus.FunctionCode(fc)}); ok {
			return d, segs
		}
	}
	fail("点表里没有 FC%02d 能读的点", fcs[0])
	return probe.Def{}, nil
}

// probeTotal 是进度的总数和写法：按寄存器数；按点表分段只算点占的寄存器，逐点独立验证和快速排查按请求数。
func probeTotal(mode probe.Mode, d probe.Def, segs []probe.Segment) (int, string) {
	switch mode {
	case probe.ModePoints:
		total := 0
		for _, s := range segs {
			total += s.N
		}
		return total, "已检测 %d / %d"
	case probe.ModeEachPoint:
		total := len(segs)
		for _, s := range segs {
			total += len(s.Points)
		}
		return total, "已发请求 %d / %d（先整段，后逐点）"
	case probe.ModeQuick:
		batch := modbus.MaxReadRegisters
		if d.Function == modbus.FuncReadCoils || d.Function == modbus.FuncReadDiscreteInputs {
			batch = modbus.MaxReadBits
		}
		return (d.Qty+batch-1)/batch + probe.QuickBudget, "已发请求 %d / 最多 %d（先批量，后抽查）"
	}
	return d.Qty, "已检测 %d / %d"
}

// probeClient 是检测用的连接。网络设备用独立的检测连接（probe.NetClient）：断线自己重连，
// Modbus TCP 超时后换新连接；串口直接用普通客户端。收发的报文都记进检测记录，导出报告用。
func probeClient(ctx context.Context, m modbus.Mode, log *probe.Log) (probe.Client, func()) {
	if *port != "" || *target == "" {
		c := connect(ctx, 0, log.Frame)
		return c, func() { c.Close() }
	}
	first, err := transport.DialTCP(ctx, *target, dialTimeout())
	if err != nil {
		fail("连接失败：%v", err)
	}
	opts := modbus.DefaultOptions(m)
	opts.Timeout, opts.DiscardEcho, opts.ReadRetries = *timeout, *echo, 0
	opts.Observer = observer(log.Frame)
	c := &probe.NetClient{Options: opts, Dial: func(ctx context.Context) (modbus.Transport, error) {
		if t := first; t != nil {
			first = nil
			return t, nil
		}
		return transport.DialTCP(ctx, *target, dialTimeout())
	}}
	if !*jsonOut {
		// Modbus TCP 超时后会主动换新连接，一般马上就连上；重连超过 1 秒才提示
		var slow *time.Timer
		c.Waiting = func(on bool) {
			switch {
			case on && first == nil:
				slow = time.AfterFunc(time.Second, func() { fmt.Fprintln(os.Stderr, "检测连接断开，正在重连（最多等 30 秒，Ctrl+C 停止）…") })
			case !on && slow != nil:
				slow.Stop()
			}
		}
	}
	return c, c.Close
}

// endpoint 是检测报告里写的连接入口：协议和地址，或串口参数。
func endpoint(m modbus.Mode) string {
	if *port != "" {
		return fmt.Sprintf("%s %s %d %d%s%d", modeName(m), *port, *baud, *dataBit, strings.ToUpper(*parity), *stop)
	}
	return modeName(m) + " " + *target
}
