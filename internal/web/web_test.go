package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/simulator"
)

const testPassword = "test-口令"

func init() {
	failDelay = 0
	reconnectEvery = 100 * time.Millisecond
}

type testEnv struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	c   *http.Client
	dir string
}

func newEnv(t *testing.T) *testEnv {
	dir := t.TempDir()
	rec, err := recorder.OpenDaily(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{Version: "test", Password: testPassword, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	jar, _ := cookiejar.New(nil)
	e := &testEnv{t, srv, ts, &http.Client{Jar: jar, Timeout: 30 * time.Second}, dir}
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
		rec.Close()
	})
	return e
}

// call 发一个 JSON 请求，返回状态码，响应解到 out。
func (e *testEnv) call(method, path string, in, out any) int {
	e.t.Helper()
	var body io.Reader
	if method != "GET" {
		if in == nil {
			in = struct{}{}
		}
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s：%v\n%s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

func (e *testEnv) login() {
	e.t.Helper()
	if code := e.call("POST", "/api/login", map[string]string{"password": testPassword}, nil); code != 200 {
		e.t.Fatalf("登录 %d", code)
	}
}

// wait 反复取状态，直到 ok 返回 true。
func (e *testEnv) wait(what string, ok func(stateView) bool) stateView {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var st stateView
		e.call("GET", "/api/state", nil, &st)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(st, "", " ")
			e.t.Fatalf("等不到%s：\n%s", what, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLoginRequiredAndRateLimited(t *testing.T) {
	e := newEnv(t)
	if code := e.call("GET", "/api/state", nil, nil); code != 401 {
		t.Fatalf("未登录取状态 %d，应为 401", code)
	}
	var session struct {
		LoggedIn bool `json:"loggedIn"`
	}
	e.call("GET", "/api/session", nil, &session)
	if session.LoggedIn {
		t.Fatal("还没登录")
	}
	e.login()
	if code := e.call("GET", "/api/state", nil, nil); code != 200 {
		t.Fatalf("登录后取状态 %d", code)
	}
	e.call("POST", "/api/logout", nil, nil)
	if code := e.call("GET", "/api/state", nil, nil); code != 401 {
		t.Fatalf("退出后取状态 %d，应为 401", code)
	}
	for i := 0; i < maxFails; i++ {
		if code := e.call("POST", "/api/login", map[string]string{"password": "wrong"}, nil); code != 401 {
			t.Fatalf("口令错误 %d", code)
		}
	}
	if code := e.call("POST", "/api/login", map[string]string{"password": testPassword}, nil); code != 429 {
		t.Fatalf("连续输错后应暂时拒绝，得到 %d", code)
	}
}

func TestCrossSiteWritesRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/disconnect", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("跨站请求 %d，应为 403", resp.StatusCode)
	}
	resp, err = e.c.Post(e.ts.URL+"/api/disconnect", "application/x-www-form-urlencoded", strings.NewReader("a=1"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("表单提交 %d，应为 415", resp.StatusCode)
	}
}

func rowByName(st stateView, name string) (RowView, bool) {
	for _, tb := range st.Tables {
		for _, r := range tb.Rows {
			if r.Name == name {
				return r, true
			}
		}
	}
	return RowView{}, false
}

// 换热站示例：连接内置模拟器、按点表读出数值、写入后回读通过、报文按天记进 SQLite 并能下载。
func TestDemoReadWriteAndRecords(t *testing.T) {
	e := newEnv(t)
	e.login()
	var st stateView
	if code := e.call("POST", "/api/demo", nil, &st); code != 200 || len(st.Tables) != 3 || st.Conn.State != stateConnected {
		t.Fatalf("示例 %d：%+v", code, st.Conn)
	}
	st = e.wait("读出示例数据", func(st stateView) bool { // 三张表各自读取，都读到再检查
		r1, ok1 := rowByName(st, "二次回水温度")
		r2, ok2 := rowByName(st, "温差设定")
		return ok1 && ok2 && r1.Value != "" && r2.Value != ""
	})
	if r, _ := rowByName(st, "二次回水温度"); r.Value != "32" || r.Unit != "℃" || r.Ref != "40003" || r.Regs != "0000 4200" {
		t.Fatalf("二次回水温度 %+v", r)
	}
	if r, _ := rowByName(st, "二次温差"); r.Value != "13.2" { // INT16 132 × 0.1
		t.Fatalf("二次温差 %+v", r)
	}
	if r, _ := rowByName(st, "温差设定"); !r.Writable || r.Type != "FLOAT32" || r.Order != "CDAB" {
		t.Fatalf("温差设定应可写，类型和字节序供写入预填：%+v", r)
	}

	var res writeResult
	if code := e.call("POST", "/api/write", writeRequest{Slave: 1, Address: "346", Type: "FLOAT32", Order: "CDAB", Value: "16"}, &res); code != 200 || res.Result != "PASS" {
		t.Fatalf("写入 %d：%+v", code, res)
	}
	if res.Target != "16" || len(res.Readbacks) != 3 || res.Readbacks[2].Value != "16" {
		t.Fatalf("写入回读 %+v", res)
	}
	if code := e.call("POST", "/api/write", writeRequest{Slave: 1, Address: "346", Type: "FLOAT32", Value: "x"}, nil); code != 400 {
		t.Fatalf("非数字应拒绝，得到 %d", code)
	}
	e.wait("写入后读取表更新", func(st stateView) bool { r, _ := rowByName(st, "温差设定"); return r.Value == "16" })

	var recs struct {
		Enabled bool       `json:"enabled"`
		Files   []fileView `json:"files"`
	}
	e.call("GET", "/api/records", nil, &recs)
	if !recs.Enabled || len(recs.Files) != 1 || !recs.Files[0].Current || !regexp.MustCompile(`^packets-\d{8}-001\.sqlite3$`).MatchString(recs.Files[0].Name) {
		t.Fatalf("记录文件 %+v", recs)
	}
	time.Sleep(300 * time.Millisecond) // 记录每 200 ms 批量写一次
	resp, err := e.c.Get(e.ts.URL + "/api/records/" + recs.Files[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) || !strings.Contains(resp.Header.Get("Content-Disposition"), recs.Files[0].Name) {
		t.Fatalf("下载 %d %q", resp.StatusCode, data[:min(len(data), 32)])
	}
	saved := filepath.Join(t.TempDir(), "download.sqlite3")
	os.WriteFile(saved, data, 0o600)
	sessions, err := new(recorder.Recorder).SessionsFile(saved, 10)
	if err != nil || len(sessions) != 1 || sessions[0].Mode != modbus.ModeTCP || !strings.HasPrefix(sessions[0].Target, "内置模拟器") {
		t.Fatalf("下载的文件应能用桌面版的历史记录打开：%+v %v", sessions, err)
	}
	packets, err := new(recorder.Recorder).PacketsFile(saved, sessions[0].ID, 1000)
	if err != nil || len(packets) < 4 {
		t.Fatalf("下载的文件里应有收发报文：%d %v", len(packets), err)
	}
	for _, bad := range []string{"..%2Fsecret", "packets.db", "%2Fetc%2Fpasswd"} {
		resp, err := e.c.Get(e.ts.URL + "/api/records/" + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s 应 404，得到 %d", bad, resp.StatusCode)
		}
	}
	var ps []packetView
	e.call("GET", "/api/packets", nil, &ps)
	if len(ps) == 0 || ps[0].Dir != "TX" || ps[0].Data == "" {
		t.Fatalf("报文 %+v", ps[:min(len(ps), 2)])
	}
}

func TestTableValidation(t *testing.T) {
	e := newEnv(t)
	e.login()
	for name, cfg := range map[string]TableConfig{
		"功能码 04 配 4x 地址": {Slave: 1, Function: 4, Address: "40001", Count: 2},
		"功能码 03 配 3x 地址": {Slave: 1, Function: 3, Address: "30001", Count: 2},
		"FLOAT32 奇数个寄存器": {Slave: 1, Function: 3, Address: "0", Count: 3, Type: "FLOAT32"},
		"不认识的字节序":        {Slave: 1, Function: 3, Address: "0", Count: 2, Type: "FLOAT32", Order: "XYZ"},
		"超过 125 个寄存器":    {Slave: 1, Function: 3, Address: "0", Count: 126},
		"数量回绕":           {Slave: 1, Function: 1, Address: "0", Count: 65537},
		"功能码 06":         {Slave: 1, Function: 6, Address: "0", Count: 1},
		"间隔太短":           {Slave: 1, Function: 3, Address: "0", Count: 1, IntervalMs: 10},
	} {
		if code := e.call("POST", "/api/tables", cfg, nil); code != 400 {
			t.Errorf("%s：应拒绝，得到 %d", name, code)
		}
	}
	var tv tableView
	if code := e.call("POST", "/api/tables", TableConfig{Slave: 1, Function: 3, Address: "40001", Count: 4, Type: "float32", Order: "cdab"}, &tv); code != 200 {
		t.Fatalf("新建 %d", code)
	}
	if tv.Config.Type != "FLOAT32" || tv.Config.Order != "CDAB" || tv.Title != "Slave 1 · FC03 · 40001 × 4 · FLOAT32 CDAB" || tv.Status != "waiting" {
		t.Fatalf("读取表 %+v", tv)
	}
	e.wait("未连接时提示", func(st stateView) bool { return len(st.Tables) == 1 && st.Tables[0].Status == "offline" })
	if code := e.call("DELETE", "/api/tables/99", nil, nil); code != 404 {
		t.Fatalf("删不存在的表 %d", code)
	}
}

// 设备断开后自动重连，读取表恢复，断开和重连都记进日志。
func TestReconnectAfterDrop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	sim := simulator.NewServer(modbus.ModeTCP, 1, simulator.HeatStation())
	go sim.Serve(ln)

	e := newEnv(t)
	e.login()
	if code := e.call("POST", "/api/connect", ConnConfig{Mode: "tcp", Target: addr, TimeoutMs: 300}, nil); code != 200 {
		t.Fatalf("连接 %d", code)
	}
	e.call("POST", "/api/tables", TableConfig{Slave: 1, Function: 3, Address: "0", Count: 2, Type: "FLOAT32", Order: "CDAB", IntervalMs: 100}, nil)
	e.wait("读出数据", func(st stateView) bool { return len(st.Tables) == 1 && st.Tables[0].Status == "ok" })

	sim.Close()
	e.wait("发现断开", func(st stateView) bool { return st.Conn.State == stateReconnecting })
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("端口 %s 被占用，无法模拟设备恢复：%v", addr, err)
	}
	sim = simulator.NewServer(modbus.ModeTCP, 1, simulator.HeatStation())
	go sim.Serve(ln)
	defer sim.Close()
	st := e.wait("重连并恢复读取", func(st stateView) bool {
		return st.Conn.State == stateConnected && st.Tables[0].Status == "ok" && st.Tables[0].Rows[0].Value == "45.2"
	})
	var kinds []string
	for _, l := range st.Logs {
		kinds = append(kinds, l.Kind)
	}
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, "DISCONNECT") || !strings.Contains(joined, "RECONNECT") {
		t.Fatalf("日志应有断开和重连：%v", kinds)
	}
	if code := e.call("POST", "/api/disconnect", nil, nil); code != 200 {
		t.Fatalf("断开 %d", code)
	}
	e.wait("断开后不再重连", func(st stateView) bool { return st.Conn.State == stateDisconnected && st.Tables[0].Status == "offline" })
}

// 识别协议先试第一个读取表的 Slave ID：现场设备常常不是 1。
func TestDetectTriesTableSlave(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sim := simulator.NewServer(modbus.ModeRTUOverTCP, 3, simulator.HeatStation())
	go sim.Serve(ln)
	defer sim.Close()
	e := newEnv(t)
	e.login()
	e.call("POST", "/api/tables", TableConfig{Slave: 3, Function: 3, Address: "0", Count: 2}, nil)
	var out struct {
		OK    bool   `json:"ok"`
		Mode  string `json:"mode"`
		Slave int    `json:"slave"`
		Error string `json:"error"`
	}
	if code := e.call("POST", "/api/detect", map[string]any{"target": ln.Addr().String(), "timeoutMs": 300}, &out); code != 200 || !out.OK || out.Mode != "rtu-over-tcp" || out.Slave != 3 {
		t.Fatalf("识别 %d %+v", code, out)
	}
}

func TestConnectErrors(t *testing.T) {
	e := newEnv(t)
	e.login()
	for name, cfg := range map[string]ConnConfig{
		"不支持的协议":   {Mode: "udp", Target: "127.0.0.1:502"},
		"缺少端口":     {Mode: "tcp", Target: "127.0.0.1"},
		"串口没选":     {Mode: "rtu"},
		"串口不能用模拟器": {Mode: "rtu", Port: "/dev/ttyUSB0", Simulator: true},
		"连不上":      {Mode: "tcp", Target: "127.0.0.1:1", TimeoutMs: 200},
		"超时太短":     {Mode: "tcp", Target: "127.0.0.1:502", TimeoutMs: 10},
	} {
		var out map[string]string
		if code := e.call("POST", "/api/connect", cfg, &out); code != 502 || out["error"] == "" {
			t.Errorf("%s：%d %v", name, code, out)
		}
	}
	st := e.wait("断开状态", func(st stateView) bool { return st.Conn.State == stateDisconnected })
	if st.Conn.Error == "" {
		t.Fatal("连不上时应显示原因")
	}
}

// 页面打开后先收到一次完整状态，之后靠推送更新。
func TestEventStream(t *testing.T) {
	e := newEnv(t)
	e.login()
	resp, err := e.c.Get(e.ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	var got []string
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.call("POST", "/api/demo", nil, nil)
	}()
	deadline := time.After(10 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for lines.Scan() {
			if name, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
				got = append(got, name)
				if name == "packets" {
					return
				}
			}
		}
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatalf("推送 %v", got)
	}
	if len(got) < 2 || got[0] != "state" {
		t.Fatalf("推送 %v", got)
	}
}

// 刚启动没有报文时是 []（页面据此渲染）；订阅推送带上 since 时先补发之后的报文。
func TestEventStreamReplaysPackets(t *testing.T) {
	e := newEnv(t)
	e.login()
	var raw json.RawMessage
	e.call("GET", "/api/packets", nil, &raw)
	if s := strings.TrimSpace(string(raw)); s != "[]" {
		t.Fatalf("没有报文时应为 []，得到 %s", s)
	}
	e.call("POST", "/api/demo", nil, nil)
	var ps []packetView
	e.wait("收发报文", func(stateView) bool { e.call("GET", "/api/packets", nil, &ps); return len(ps) >= 4 })
	resp, err := e.c.Get(e.ts.URL + "/api/events?since=" + strconv.FormatUint(ps[1].Seq, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	var names []string
	for lines.Scan() && len(names) <= 2 {
		if name, ok := strings.CutPrefix(lines.Text(), "event: "); ok {
			names = append(names, name)
		} else if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok && len(names) == 2 {
			if names[0] != "state" || names[1] != "packets" {
				break
			}
			var got []packetView
			if err := json.Unmarshal([]byte(data), &got); err != nil || len(got) == 0 || got[0].Seq != ps[2].Seq {
				t.Fatalf("补发应从序号 %d 开始：%v %+v", ps[2].Seq, err, got[:min(len(got), 1)])
			}
			return
		}
	}
	t.Fatalf("连上后应先收到 state 再收到补发的 packets，得到 %v", names)
}

func TestStaticPage(t *testing.T) {
	e := newEnv(t)
	resp, err := e.c.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("<html")) || bytes.Contains(body, []byte("E-SafeNet")) {
		t.Fatalf("首页 %d %q", resp.StatusCode, body[:min(len(body), 64)])
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}
}
