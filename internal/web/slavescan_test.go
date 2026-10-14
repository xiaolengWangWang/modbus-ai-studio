package web

import (
	"strings"
	"testing"
)

// waitScan 等搜索结束，返回推送状态里的结果。
func (e *testEnv) waitScan(id int) scanView {
	e.t.Helper()
	st := e.wait("搜索结束", func(st stateView) bool { return st.Scan != nil && st.Scan.ID == id && !st.Scan.Running })
	return *st.Scan
}

// 搜索站号：和桌面版、modbus-cli 同一套算法。示例模拟器只应答 Slave 1；搜索期间不能开始检测，读取表暂停，搜完继续；可以停止。
func TestScanSlaves(t *testing.T) {
	e := newEnv(t)
	e.login()
	if code := e.call("POST", "/api/demo", nil, nil); code != 200 {
		t.Fatalf("示例 %d", code)
	}
	e.wait("示例读出数据", func(st stateView) bool { return len(st.Tables) > 0 && st.Tables[0].Status == "ok" })

	for _, bad := range []scanRequest{{From: 0, To: 3, TimeoutMS: 100}, {From: 1, To: 3, TimeoutMS: 5}, {From: 1, To: 3, TimeoutMS: 100, Address: "abc"}} {
		if code := e.call("POST", "/api/slavescan", bad, nil); code != 400 {
			t.Errorf("%+v 应报参数错误：%d", bad, code)
		}
	}

	var v scanView
	if code := e.call("POST", "/api/slavescan", scanRequest{From: 1, To: 5, TimeoutMS: 100, Address: "40101"}, &v); code != 202 || !v.Running {
		t.Fatalf("开始搜索 %d：%+v", code, v)
	}
	if len(v.Probes) != 4 || v.Probes[0] != "40101（FC03）" {
		t.Errorf("每个站号应先读给的地址，再读 40001、30001、线圈 1：%v", v.Probes)
	}
	if code := e.call("POST", "/api/probe", probeRequest{How: "auto", Slave: 1, Function: 3, Address: "40001", Count: 10}, nil); code != 409 {
		t.Errorf("搜索期间不能开始检测：%d", code)
	}
	v = e.waitScan(v.ID)
	if v.Error != "" || v.Stopped || v.Done != 5 || len(v.Found) != 1 || v.Found[0].Slave != 1 || v.Found[0].Probe != "40101（FC03）" ||
		!strings.Contains(v.Found[0].Note, "正常响应") {
		t.Fatalf("应只找到 Slave 1：%+v", v)
	}
	st := e.wait("搜完读取表继续读", func(st stateView) bool { return st.Tables[0].Status == "ok" })
	if log := logText(st); !strings.Contains(log, "Slave 1–5：开始，读取表暂停") || !strings.Contains(log, "Slave 1–5：找到 Slave 1") {
		t.Errorf("日志应记下开始和结果：%s", log)
	}

	// 停止：保留已搜到的站号
	if code := e.call("POST", "/api/slavescan", scanRequest{From: 1, To: 247, TimeoutMS: 100}, &v); code != 202 {
		t.Fatalf("开始搜索 %d", code)
	}
	if code := e.call("POST", "/api/slavescan/stop", nil, nil); code != 200 {
		t.Fatalf("停止 %d", code)
	}
	v = e.waitScan(v.ID)
	if !v.Stopped || v.Done >= v.Total || v.Error != "" {
		t.Errorf("应停在中途：%+v", v)
	}
}
