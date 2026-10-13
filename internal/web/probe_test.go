package web

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitProbe 等检测结束，返回完整结果。
func (e *testEnv) waitProbe(id int) probeView {
	e.t.Helper()
	e.wait("检测结束", func(st stateView) bool { return st.Probe != nil && st.Probe.ID == id && !st.Probe.Running })
	var v probeView
	if code := e.call("GET", "/api/probe", nil, &v); code != 200 {
		e.t.Fatalf("取检测结果 %d", code)
	}
	return v
}

// 检测寄存器：和桌面版同一套算法；网络设备检测期间读取表暂停、让出常规连接，结束后马上恢复。
func TestProbeRegisters(t *testing.T) {
	e := newEnv(t)
	e.login()
	if code := e.call("POST", "/api/demo", nil, nil); code != 200 {
		t.Fatalf("示例 %d", code)
	}
	e.wait("示例读出数据", func(st stateView) bool { return len(st.Tables) > 0 && st.Tables[0].Status == "ok" })

	// 模拟器有 1000 个保持寄存器：40991–41000 读得通，41001–41010 是非法地址
	var b probeBrief
	if code := e.call("POST", "/api/probe", probeRequest{How: "auto", Slave: 1, Function: 3, Address: "40991", Count: 20}, &b); code != 202 || !b.Running {
		t.Fatalf("开始检测 %d：%+v", code, b)
	}
	v := e.waitProbe(b.ID)
	if v.Error != "" || !v.Problems || v.Readable == nil || v.Readable.Address != "40991" || v.Readable.Count != 10 {
		t.Fatalf("自动定位：%+v", v)
	}
	if len(v.Rows) != 10 || v.Rows[0][0] != "41001" || !strings.Contains(v.Rows[0][2], "非法地址") || !strings.Contains(v.Text, "非法地址（10，异常 02）：41001–41010") {
		t.Errorf("失败清单应是 41001–41010：%v\n%s", v.Rows, v.Text)
	}
	if !strings.Contains(v.Summary, "非法地址 10") || v.Requests == 0 {
		t.Errorf("摘要：%q，请求 %d", v.Summary, v.Requests)
	}
	st := e.wait("检测完恢复常规连接、读取表继续读", func(st stateView) bool {
		return st.Conn.State == stateConnected && st.Tables[0].Status == "ok"
	})
	if !strings.Contains(logText(st), "检测寄存器") && !strings.Contains(logText(st), "自动定位") {
		t.Errorf("日志应记下检测：%s", logText(st))
	}

	// 报告：Markdown、JSONL、CSV 打成一个 zip
	resp, err := e.c.Get(e.ts.URL + "/api/probe/report?note=" + "经DTU转发")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if resp.StatusCode != 200 || err != nil || len(zr.File) != 3 {
		t.Fatalf("报告 %d：%v，%d 个文件", resp.StatusCode, err, len(zr.File))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".zip") {
		t.Errorf("下载文件名：%s", cd)
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".md") {
			rc, _ := f.Open()
			md, _ := io.ReadAll(rc)
			rc.Close()
			if !strings.Contains(string(md), "经DTU转发") || !strings.Contains(string(md), "41001") {
				t.Errorf("报告正文：\n%s", md)
			}
		}
	}
}

func logText(st stateView) string {
	var b strings.Builder
	for _, l := range st.Logs {
		b.WriteString(l.Kind + " " + l.Detail + "\n")
	}
	return b.String()
}

// 按点表检测、停止、出错的情况。
func TestProbePointsStopAndErrors(t *testing.T) {
	e := newEnv(t)
	e.login()
	if code := e.call("POST", "/api/probe", probeRequest{How: "auto", Slave: 1, Function: 3, Address: "40001", Count: 10}, nil); code != 409 {
		t.Errorf("没连接时不能检测：%d", code)
	}
	if code := e.call("POST", "/api/demo", nil, nil); code != 200 {
		t.Fatalf("示例 %d", code)
	}
	e.wait("连上", func(st stateView) bool { return st.Conn.State == stateConnected })
	for _, bad := range []probeRequest{
		{How: "auto", Slave: 0, Function: 3, Address: "40001", Count: 10},
		{How: "nope", Slave: 1, Function: 3, Address: "40001", Count: 10},
		{How: "auto", Slave: 1, Function: 3, Address: "40001", Count: 0},
		{How: "each", Slave: 1, Function: 2},
	} {
		if code := e.call("POST", "/api/probe", bad, nil); code != 400 {
			t.Errorf("%+v 应拒绝：%d", bad, code)
		}
	}

	table := "地址,名称,类型,字节序,倍率,单位,读写\n40001,二次供水温度,FLOAT32,CDAB,1,℃,R\n40005,二次温差,INT16,,0.1,℃,R\n41000,末尾,UINT16,,,,R\n41001,越界,UINT16,,,,R\n"
	if code := e.call("POST", "/api/points", importRequest{Name: "点表.csv", Data: []byte(table)}, nil); code != 200 {
		t.Fatalf("导入 %d", code)
	}
	var b probeBrief
	if code := e.call("POST", "/api/probe", probeRequest{How: "each", Slave: 1, Function: 3}, &b); code != 202 {
		t.Fatalf("逐点独立验证 %d", code)
	}
	v := e.waitProbe(b.ID)
	if v.Error != "" || len(v.Rows) != 1 || v.Rows[0][2] != "越界" || v.Readable != nil {
		t.Errorf("只有“越界”读不通：%+v", v)
	}

	// 停止：慢慢逐个读一大段，开始后马上停，保留已完成的结果
	if code := e.call("POST", "/api/probe", probeRequest{How: "single", Slave: 1, Function: 3, Address: "40001", Count: 2000}, &b); code != 202 {
		t.Fatalf("逐个寄存器 %d", code)
	}
	if code := e.call("POST", "/api/probe", probeRequest{How: "auto", Slave: 1, Function: 3, Address: "40001", Count: 10}, nil); code != http.StatusConflict {
		t.Errorf("检测中不能再开一个：%d", code)
	}
	time.Sleep(100 * time.Millisecond)
	e.call("POST", "/api/probe/stop", nil, nil)
	v = e.waitProbe(b.ID)
	if !strings.Contains(v.Error, "已停止") || v.Done >= 2000 {
		t.Errorf("停止后：%+v", v.probeBrief)
	}
	e.wait("停止后恢复读取", func(st stateView) bool { return st.Conn.State == stateConnected && st.Tables[0].Status == "ok" })
}
