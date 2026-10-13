package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modbus-ai-studio/internal/control"
	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/points"
)

// 按点表读写：-points 指定点表文件（CSV / xlsx，本程序的点表或平台导出的设备属性表，和桌面版导入点表一样），
// 之后用点名读写，按点表里的类型、字节序、倍率、单位、枚举换算工程值。

// pointInfo 是点表里的一个点，JSON 输出用。
type pointInfo struct {
	Name    string         `json:"name"`
	Address string         `json:"address"`
	Type    string         `json:"type"`
	Order   string         `json:"order,omitempty"`
	Scale   float64        `json:"scale"`
	Unit    string         `json:"unit,omitempty"`
	RW      bool           `json:"writable"`
	Min     *float64       `json:"min,omitempty"`
	Max     *float64       `json:"max,omitempty"`
	Enum    map[int]string `json:"enum,omitempty"`
	Len     int            `json:"length,omitempty"`
}

func infoOf(p points.Point) pointInfo {
	return pointInfo{Name: p.Name, Address: modbus.Reference(p.Area, p.Offset), Type: string(p.Type), Order: string(p.Order),
		Scale: p.Scale, Unit: p.Unit, RW: p.RW, Min: p.Min, Max: p.Max, Enum: p.Enum, Len: p.Len}
}

// loadPoints 读 -points 指定的点表。
func loadPoints() (points.Import, points.Table) {
	if *pointsFile == "" {
		fail("这条命令要用 -points 指定点表文件（CSV 或 xlsx）")
	}
	data, err := os.ReadFile(*pointsFile)
	if err != nil {
		fail("读点表：%v", err)
	}
	imp, err := points.ParseFile(filepath.Base(*pointsFile), data)
	if err != nil {
		fail("点表 %s：%v", filepath.Base(*pointsFile), err)
	}
	return imp, points.NewTable(imp.Points)
}

// findPoint 按点名找点：先找全名，再找唯一包含这几个字的点；也可以直接写地址（点名重复时用）。
func findPoint(t points.Table, name string) points.Point {
	list := t.List()
	var exact, partial []points.Point
	for _, p := range list {
		switch {
		case p.Name == name:
			exact = append(exact, p)
		case strings.Contains(p.Name, name):
			partial = append(partial, p)
		}
	}
	names := func(ps []points.Point) string {
		var out []string
		for _, p := range ps[:min(len(ps), 8)] {
			out = append(out, p.Name+"（"+modbus.Reference(p.Area, p.Offset)+"）")
		}
		return strings.Join(out, "、")
	}
	switch {
	case len(exact) == 1:
		return exact[0]
	case len(exact) > 1:
		fail("有 %d 个点叫“%s”：%s，改用地址指定", len(exact), name, names(exact))
	case len(partial) == 1:
		return partial[0]
	case len(partial) > 1:
		fail("“%s”对上了 %d 个点：%s，写全名", name, len(partial), names(partial))
	}
	if cands, err := modbus.ParseAddress(name); err == nil {
		for _, c := range cands {
			area := c.Area
			if area == modbus.AreaNone {
				area = modbus.AreaHoldingRegisters
			}
			if p, ok := t.Get(area, c.Offset); ok {
				return p
			}
		}
	}
	fail("点表里没有“%s”，运行 modbus-cli -points %s points 查看全部点", name, *pointsFile)
	return points.Point{}
}

// ---- points：列出点表 ----

type pointsResult struct {
	OK      bool        `json:"ok"`
	File    string      `json:"file"`
	Format  string      `json:"format"`
	Count   int         `json:"count"`
	Points  []pointInfo `json:"points"`
	Skipped []string    `json:"skipped,omitempty"` // 导入时跳过的行和原因
	Notes   []string    `json:"notes,omitempty"`
}

func runPoints() {
	imp, t := loadPoints()
	res := pointsResult{OK: true, File: filepath.Base(*pointsFile), Format: imp.Format, Points: []pointInfo{}, Skipped: imp.Skipped, Notes: imp.Notes}
	for _, p := range t.List() {
		res.Points = append(res.Points, infoOf(p))
	}
	res.Count = len(res.Points)
	output(res, func() {
		fmt.Printf("%s（%s）：%d 个点\n\n", res.File, res.Format, res.Count)
		fmt.Printf("%-8s %-10s %-6s %-6s %-6s %s\n", "地址", "类型", "字节序", "读写", "单位", "名称")
		for _, p := range res.Points {
			rw := "R"
			if p.RW {
				rw = "RW"
			}
			fmt.Printf("%-8s %-10s %-6s %-6s %-6s %s\n", p.Address, p.Type, p.Order, rw, p.Unit, p.Name)
		}
		if len(res.Skipped) > 0 {
			fmt.Printf("\n跳过 %d 行：\n  %s\n", len(res.Skipped), strings.Join(res.Skipped[:min(len(res.Skipped), 12)], "\n  "))
		}
	})
}

// ---- get：按点名读 ----

type pointValue struct {
	Name      string   `json:"name"`
	Address   string   `json:"address"`
	Value     string   `json:"value,omitempty"` // 工程值，带枚举含义；64 位整数不丢精度
	Unit      string   `json:"unit,omitempty"`
	Registers []string `json:"registers,omitempty"`
	Hint      string   `json:"hint,omitempty"`  // 数值不合理时的提示
	Error     string   `json:"error,omitempty"` // 这个点所在的一段没读出来
}

type getResult struct {
	OK        bool         `json:"ok"`
	Error     string       `json:"error,omitempty"`
	Mode      string       `json:"mode"`
	Slave     uint         `json:"slave"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Values    []pointValue `json:"values"`
}

// runGet 读点：没写点名时读点表里的全部点。相近的点合成一次读取（和桌面版建读取窗口一样），
// 某一段读不出来时只标出那一段的点，其他照常读。
func runGet(ctx context.Context, names []string) {
	_, t := loadPoints()
	selected := t.List()
	if len(names) > 0 {
		selected = nil
		for _, n := range names {
			selected = append(selected, findPoint(t, n))
		}
	}
	c := connect(ctx, 1)
	defer c.Close()
	start := time.Now()
	res := getResult{OK: true, Mode: modeName(c.Mode()), Slave: *slave}
	got := map[points.Key]pointValue{}
	for _, r := range points.Ranges(selected) {
		resp, err := c.Do(ctx, modbus.Request{Slave: byte(*slave), Function: r.Area.ReadFunction(), Address: r.Start, Quantity: uint16(r.Count)})
		for _, p := range selected {
			if p.Area != r.Area || p.Offset < r.Start || int(p.Offset)+p.Regs() > int(r.Start)+r.Count {
				continue
			}
			v := pointValue{Name: p.Name, Address: modbus.Reference(p.Area, p.Offset), Unit: p.Unit}
			if err != nil {
				v.Error = errorText(err)
				res.OK = false
				got[points.Key{Area: p.Area, Offset: p.Offset}] = v
				continue
			}
			i := int(p.Offset - r.Start)
			var regs []uint16
			if r.Area == modbus.AreaCoils || r.Area == modbus.AreaDiscreteInputs {
				regs = []uint16{0}
				if resp.Bits[i] {
					regs[0] = 1
				}
			} else {
				regs = resp.Registers[i : i+p.Regs()]
				for _, x := range regs {
					v.Registers = append(v.Registers, fmt.Sprintf("%04X", x))
				}
			}
			text, plausible, derr := p.Text(regs)
			switch {
			case derr != nil:
				v.Error = derr.Error()
				res.OK = false
			case !plausible:
				v.Hint = "数值不合理，字节序可能不对"
				if sug, ok := modbus.SuggestByteOrder(p.Order, regs); ok && p.Type == modbus.TypeFloat32 {
					v.Hint = fmt.Sprintf("数值不合理，按 %s 解码更可能正确", sug)
				}
			}
			v.Value = text
			got[points.Key{Area: p.Area, Offset: p.Offset}] = v
		}
	}
	for _, p := range selected { // 按点名或点表的顺序输出
		res.Values = append(res.Values, got[points.Key{Area: p.Area, Offset: p.Offset}])
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if !res.OK {
		res.Error = "有点没读出来，见各点的 error"
	}
	output(res, func() {
		fmt.Printf("%s · Slave %d · %d 个点 · %d ms\n\n", res.Mode, res.Slave, len(res.Values), res.ElapsedMS)
		for _, v := range res.Values {
			val := strings.TrimSpace(v.Value + " " + v.Unit)
			switch {
			case v.Error != "":
				val = "读取失败：" + v.Error
			case v.Hint != "":
				val += "（" + v.Hint + "）"
			}
			fmt.Printf("%-8s %s：%s\n", v.Address, v.Name, val)
		}
	})
	if !res.OK {
		os.Exit(1)
	}
}

// errorText 是通信错误的简短说明，异常响应带上建议。
func errorText(err error) string {
	if ex, ok := modbus.AsException(err); ok {
		return fmt.Sprintf("%v。建议：%s", ex, ex.Code.Tip())
	}
	return err.Error()
}

// ---- set：按点名写 ----

type readbackValue struct {
	AtMS  int64  `json:"at_ms"`
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type setResult struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Name      string          `json:"name"`
	Address   string          `json:"address"`
	Original  string          `json:"original"` // 写入前的值
	Target    string          `json:"target"`   // 实际写入的值（取整后）
	Unit      string          `json:"unit,omitempty"`
	Result    string          `json:"result"` // PASS、NOT_APPLIED（未生效）、OVERWRITTEN（被改回）、MISMATCH（值不符）、UNKNOWN、PROTOCOL_ERROR
	Hint      string          `json:"hint,omitempty"`
	Readbacks []readbackValue `json:"readbacks"`
}

var resultNames = map[control.Result]string{
	control.ResultPass: "通过", control.ResultNotApplied: "未生效", control.ResultOverwritten: "被改回",
	control.ResultMismatch: "值不符", control.ResultUnknown: "无法判断", control.ResultProtocolError: "设备拒绝或读原值失败",
}

// runSet 写一个点：按点表的类型、字节序、倍率编码，检查可写和允许范围；枚举点可以写含义（例如“自动”）。
// 写入后在 0、200、1000 ms 回读，判断通过、未生效、被改回还是值不符，和桌面版的控制验证一样。
func runSet(ctx context.Context, name, text string) {
	_, t := loadPoints()
	p := findPoint(t, name)
	switch {
	case !p.RW:
		fail("“%s”在点表里不是可写点（读写列没有 W）", p.Name)
	case p.Type == points.TypeString:
		fail("“%s”是字符串点，不能用 set 写", p.Name)
	case p.Area != modbus.AreaCoils && p.Area != modbus.AreaHoldingRegisters:
		fail("“%s”在 %s 区，只读", p.Name, p.Area.Prefix())
	}
	tg := control.Target{Slave: byte(*slave), Area: p.Area, Address: p.Offset, Type: p.Type, ReadOrder: p.Order, Scaling: p.Scaling()}
	text = strings.TrimSpace(text)
	var regs []uint16
	var value float64
	var err error
	if k, ok := enumValue(p, text); ok {
		value = float64(k)
		regs, _, err = control.Encode(tg, value)
	} else {
		if p.Area == modbus.AreaCoils {
			if v, ok := map[string]string{"on": "1", "off": "0", "true": "1", "false": "0"}[strings.ToLower(text)]; ok {
				text = v
			}
		}
		regs, value, _, err = control.EncodeText(tg, text)
	}
	if err != nil {
		fail("“%s”的值：%v", p.Name, err)
	}
	if lo, hi, ok := p.Limits(); ok && (value < lo || value > hi) {
		fail("“%s”允许 %v–%v，%v 超出范围", p.Name, lo, hi, value)
	}
	c := connect(ctx, 1)
	defer c.Close()
	rep := control.Verify(ctx, c, tg, regs, nil)
	res := setResult{OK: rep.Result == control.ResultPass, Name: p.Name, Address: modbus.Reference(p.Area, p.Offset), Unit: p.Unit,
		Original: p.Format(rep.Original), Target: p.Format(rep.Target), Result: string(rep.Result), Hint: rep.Hint, Readbacks: []readbackValue{}}
	if rep.WriteErr != nil {
		res.Hint = strings.TrimPrefix(res.Hint+"；写入："+errorText(rep.WriteErr), "；")
	}
	for _, rb := range rep.Readbacks {
		v := readbackValue{AtMS: rb.At.Milliseconds()}
		if rb.Err != nil {
			v.Error = rb.Err.Error()
		} else {
			v.Value = p.Format(rb.Value)
		}
		res.Readbacks = append(res.Readbacks, v)
	}
	if !res.OK {
		res.Error = "控制验证没有通过：" + resultNames[rep.Result]
	}
	output(res, func() {
		fmt.Println(strings.TrimSpace(fmt.Sprintf("%s（%s）：%s → %s %s", res.Name, res.Address, res.Original, res.Target, res.Unit)))
		for _, rb := range res.Readbacks {
			if rb.Error != "" {
				fmt.Printf("  %4d ms 回读失败：%s\n", rb.AtMS, rb.Error)
			} else {
				fmt.Printf("  %4d ms 回读 %s\n", rb.AtMS, rb.Value)
			}
		}
		fmt.Printf("结论：%s", resultNames[rep.Result])
		if res.Hint != "" {
			fmt.Printf("（%s）", res.Hint)
		}
		fmt.Println()
	})
	if !res.OK {
		os.Exit(1)
	}
}

// enumValue 按枚举含义找值：写“自动”等于写 1；数字照常按数值处理。
func enumValue(p points.Point, text string) (int, bool) {
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return 0, false
	}
	keys := make([]int, 0, len(p.Enum))
	for k := range p.Enum {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if p.Enum[k] == text {
			return k, true
		}
	}
	return 0, false
}
