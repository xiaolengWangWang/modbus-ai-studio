package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// faulty 启动 Slave 1 的 Modbus TCP 模拟器：100 个保持寄存器，offset 列表里的地址回异常 02。
func faulty(t *testing.T, offsets ...uint16) string {
	t.Helper()
	srv := simulator.NewServer(modbus.ModeTCP, 1, simulator.NewStore(100))
	var f simulator.Faults
	for _, off := range offsets {
		f.Exceptions = append(f.Exceptions, simulator.ExceptionRange{AddrRange: simulator.AddrRange{Start: off, Count: 1}, Code: modbus.ExceptionIllegalDataAddress})
	}
	srv.SetFaults(f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// failed 是失败清单里的地址和结果。
func failed(v map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := v["failures"].([]any)
	for _, x := range list {
		f := x.(map[string]any)
		out[f["地址"].(string)] = f["结果"].(string)
	}
	return out
}

// 自动定位：批量读通的段不逐个读，读不通的段定位到单个地址；超出设备范围的地址同样是非法地址。
func TestProbeLocatesIllegalAddresses(t *testing.T) {
	addr := faulty(t, 5)
	v, code := jsonCLI(t, "-target", addr, "probe", "40001", "20")
	if code != 0 || v["ok"] != true || v["mode"] != "auto" || v["problems"] != true || v["count"] != float64(20) {
		t.Fatalf("退出码 %d：%v", code, v)
	}
	if f := failed(v); len(f) != 1 || !strings.Contains(f["40006"], "非法地址") {
		t.Errorf("只有 40006 读不通：%v", f)
	}
	if r, _ := v["readable"].(map[string]any); r["start"] != "40007" || r["count"] != float64(14) {
		t.Errorf("最长可读范围应是 40007 起 14 个：%v", v["readable"])
	}
	v, code = jsonCLI(t, "-target", addr, "probe", "single", "40096", "10")
	if f := failed(v); code != 0 || len(f) != 5 || f["40101"] == "" || f["40100"] != "" {
		t.Errorf("逐个寄存器：40101–40105 超出设备范围：退出码 %d，%v", code, f)
	}
	if n := v["requests"].(float64); n != 10 {
		t.Errorf("逐个寄存器每个地址读一次：%v 次请求", n)
	}
	out, code := cli(t, "-target", addr, "probe", "40001", "10")
	if code != 0 || !strings.Contains(out, "非法地址（1，异常 02）：40006") || !strings.Contains(out, "常规读取可以改用：40001 起 5 个") {
		t.Errorf("文字结果：\n%s", out)
	}
}

// 按点表分段、逐点独立验证：结果按点列出，报告写出 Markdown、JSONL、CSV 三个文件。
func TestProbePointTableAndReport(t *testing.T) {
	addr := faulty(t, 10)
	dir := t.TempDir()
	file := filepath.Join(dir, "点表.csv")
	table := "地址,名称,类型,字节序,倍率,单位,读写\n" +
		"40001,供水温度,FLOAT32,CDAB,1,℃,R\n40003,回水温度,FLOAT32,CDAB,1,℃,R\n" +
		"40011,压差,INT16,,0.01,MPa,R\n40012,频率,UINT16,,0.1,Hz,R\n"
	if err := os.WriteFile(file, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	v, code := jsonCLI(t, "-target", addr, "-points", file, "probe", "points")
	if code != 0 || v["mode"] != "points" || v["points"] != float64(4) || v["start"] != "40001" || v["count"] != float64(12) {
		t.Fatalf("按点表分段：退出码 %d，%v", code, v)
	}
	if list, _ := v["failures"].([]any); len(list) != 1 || list[0].(map[string]any)["名称"] != "压差" {
		t.Errorf("只有“压差”读不通：%v", v["failures"])
	}
	report := filepath.Join(dir, "检测报告.md")
	v, code = jsonCLI(t, "-target", addr, "-points", file, "-report", report, "-report-note", "经 DTU 转发", "probe", "each")
	files, _ := v["report"].([]any)
	if code != 0 || v["mode"] != "each" || len(files) != 3 {
		t.Fatalf("逐点独立验证带报告：退出码 %d，%v", code, v)
	}
	for _, f := range files {
		if st, err := os.Stat(f.(string)); err != nil || st.Size() == 0 {
			t.Errorf("报告文件 %v：%v", f, err)
		}
	}
	if md, _ := os.ReadFile(report); !strings.Contains(string(md), "经 DTU 转发") || !strings.Contains(string(md), "压差") {
		t.Errorf("报告应写明备注和失败的点：\n%s", md)
	}
	if v, code := jsonCLI(t, "-target", addr, "-points", file, "probe", "quick", "40001", "12"); code != 0 || v["mode"] != "quick" {
		t.Errorf("快速排查：退出码 %d，%v", code, v)
	}
}

func TestProbeUsageErrors(t *testing.T) {
	addr := faulty(t)
	if _, code := jsonCLI(t, "-target", addr, "probe"); code != 2 {
		t.Errorf("probe 不写范围是用法错误：%d", code)
	}
	if v, code := jsonCLI(t, "-target", addr, "probe", "points"); code != 1 || !strings.Contains(v["error"].(string), "-points") {
		t.Errorf("按点表检测要 -points：%d %v", code, v)
	}
	if v, code := jsonCLI(t, "-target", addr, "-slave", "0", "probe", "40001", "5"); code != 1 || !strings.Contains(v["error"].(string), "广播") {
		t.Errorf("站号 0 不能检测：%d %v", code, v)
	}
}
