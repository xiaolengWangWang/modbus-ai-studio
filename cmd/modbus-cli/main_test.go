package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
)

// 测试把自己当成 modbus-cli 再运行一次：环境变量里带上命令行参数，TestMain 直接调 main。
func TestMain(m *testing.M) {
	if os.Getenv("MODBUS_CLI_RUN") == "1" {
		os.Args = append([]string{"modbus-cli"}, strings.Split(os.Getenv("MODBUS_CLI_ARGS"), "\n")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// cli 运行 modbus-cli，返回标准输出和退出码。
func cli(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MODBUS_CLI_RUN=1", "MODBUS_CLI_ARGS="+strings.Join(args, "\n"))
	out, err := cmd.Output()
	code := 0
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// jsonCLI 加 -json 运行，解析输出的 JSON 对象。
func jsonCLI(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	out, code := cli(t, append([]string{"-json"}, args...)...)
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v 的输出不是一个 JSON 对象：%v\n%s", args, err, out)
	}
	return v, code
}

// simulate 启动 Slave 1 的 Modbus TCP 模拟器，返回地址和寄存器存储。
func simulate(t *testing.T) (string, *simulator.Store) {
	t.Helper()
	st := simulator.NewStore(1000)
	srv := simulator.NewServer(modbus.ModeTCP, 1, st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), st
}

func TestReadWriteRegisters(t *testing.T) {
	addr, st := simulate(t)
	if err := st.SetValue(modbus.AreaHoldingRegisters, 10, modbus.TypeFloat32, modbus.OrderCDAB, 25.5); err != nil {
		t.Fatal(err)
	}
	v, code := jsonCLI(t, "-target", addr, "-type", "float32", "-order", "CDAB", "read", "40011", "2")
	values, _ := v["values"].([]any)
	if code != 0 || v["ok"] != true || len(values) != 1 {
		t.Fatalf("读 FLOAT32：退出码 %d，%v", code, v)
	}
	if first := values[0].(map[string]any); first["value"] != "25.5" || first["address"] != "40011" {
		t.Errorf("40011 应读出 25.5：%v", first)
	}

	v, code = jsonCLI(t, "-target", addr, "-type", "float32", "-order", "CDAB", "write", "40021", "16")
	if code != 0 || v["ok"] != true || v["verified"] != true || v["function"] != float64(16) || v["readback"] != "16" {
		t.Fatalf("写 FLOAT32 应回读一致：退出码 %d，%v", code, v)
	}
	regs, _ := st.Registers(modbus.AreaHoldingRegisters, 20, 2)
	if raw, _ := modbus.DecodeRaw(modbus.TypeFloat32, modbus.OrderCDAB, regs); raw != 16 {
		t.Errorf("设备里应是 16：%v", raw)
	}
	if v, _ := jsonCLI(t, "-target", addr, "write", "40031", "7"); v["function"] != float64(6) {
		t.Errorf("一个寄存器默认用 06：%v", v)
	}
}

func TestCoils(t *testing.T) {
	addr, _ := simulate(t)
	if v, code := jsonCLI(t, "-target", addr, "-fc", "5", "write", "2", "on"); code != 0 || v["verified"] != true || v["function"] != float64(5) {
		t.Fatalf("写线圈：退出码 %d，%v", code, v)
	}
	v, code := jsonCLI(t, "-target", addr, "-fc", "1", "read", "1", "3")
	values, _ := v["values"].([]any)
	if code != 0 || len(values) != 3 {
		t.Fatalf("读线圈：退出码 %d，%v", code, v)
	}
	var got []string
	for _, x := range values {
		got = append(got, x.(map[string]any)["value"].(string))
	}
	if strings.Join(got, "") != "010" {
		t.Errorf("线圈 1–3 应是 0 1 0：%v", got)
	}
	if v, code := jsonCLI(t, "-target", addr, "-fc", "1", "read", "3x0001", "1"); code != 1 || !strings.Contains(v["error"].(string), "对不上") {
		t.Errorf("3x 地址配 -fc 1 应报错：%d %v", code, v)
	}
}

func TestDetectScanRaw(t *testing.T) {
	addr, _ := simulate(t)
	if v, code := jsonCLI(t, "-target", addr, "detect"); code != 0 || v["mode"] != "tcp" || v["slave"] != float64(1) {
		t.Errorf("识别协议：%d %v", code, v)
	}
	v, code := jsonCLI(t, "-target", addr, "scan", "1", "3")
	found, _ := v["found"].([]any)
	if code != 0 || len(found) != 1 || found[0].(map[string]any)["slave"] != float64(1) {
		t.Errorf("只有 Slave 1 应答：%d %v", code, v)
	}
	if v, code := jsonCLI(t, "-target", addr, "raw", "03 00 00 00 01"); code != 0 || !strings.HasPrefix(v["response"].(string), "03 02 ") {
		t.Errorf("自定义请求：%d %v", code, v)
	}
}

func TestErrorsAndExitCodes(t *testing.T) {
	addr, _ := simulate(t)
	if v, code := jsonCLI(t, "-target", addr, "read", "40001"); code != 2 || v["ok"] != false {
		t.Errorf("参数个数不对应退出码 2：%d %v", code, v)
	}
	v, code := jsonCLI(t, "-target", addr, "read", "40999", "10")
	if code != 1 || v["ok"] != false || !strings.Contains(v["error"].(string), "建议") {
		t.Errorf("超出范围应是异常响应、退出码 1，并给出建议：%d %v", code, v)
	}
	if v, code := jsonCLI(t, "-target", addr, "-fc", "6", "-type", "float32", "write", "40001", "1"); code != 1 || !strings.Contains(v["error"].(string), "-fc 16") {
		t.Errorf("FLOAT32 不能用 06：%d %v", code, v)
	}
	if v, code := jsonCLI(t, "-target", "127.0.0.1:1", "-timeout", "300ms", "read", "40001", "1"); code != 1 || !strings.Contains(v["error"].(string), "连接失败") {
		t.Errorf("连不上：%d %v", code, v)
	}
}

func TestManualAndVersion(t *testing.T) {
	out, code := cli(t, "manual")
	for _, want := range []string{"read  <地址> <数量>", "write <地址> <值>", "detect", "scan [起始 结束]", "raw <PDU 十六进制>", "-json", "-fc", "退出码", "给 AI 调用"} {
		if !strings.Contains(out, want) {
			t.Errorf("说明里缺少“%s”", want)
		}
	}
	if code != 0 || strings.Contains(out, "{{") {
		t.Errorf("说明没有展开：%d", code)
	}
	if v, _ := jsonCLI(t, "version"); v["ok"] != true || v["version"] != "dev" {
		t.Errorf("版本号：%v", v)
	}
}
