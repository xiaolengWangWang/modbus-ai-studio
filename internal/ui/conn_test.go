package ui

import (
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
)

func TestSerialPortClaim(t *testing.T) {
	if err := claimPort("COM_TEST", 1); err != nil {
		t.Fatal(err)
	}
	defer releasePort("COM_TEST")
	if err := claimPort("COM_TEST", 2); err == nil || !strings.Contains(err.Error(), "窗口 1") {
		t.Fatalf("同一串口在另一个窗口打开应报错，得到 %v", err)
	}
	if err := claimPort("COM_TEST", 1); err != nil {
		t.Fatalf("同一窗口重复占用应允许：%v", err)
	}
}

// 连接栏按 1024 宽的工控机屏幕设计：两种协议下的最小宽度都不能超过 1024。
func TestConnectionBarFits1024(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	for _, p := range protoNames {
		var w float32
		locked(func() {
			ws.proto.SetSelected(p)
			w = ws.bar.MinSize().Width
		})
		if w > 1024 {
			t.Errorf("%s 连接栏最小宽度 %.0f，超过 1024", p, w)
		}
		t.Logf("%s 连接栏最小宽度 %.0f", p, w)
	}
	var cfg connConfig
	var err error
	locked(func() {
		ws.port.SetOptions([]string{"COM3"})
		ws.port.SetSelected("COM3")
		ws.frameFmt.SetSelected("8E1")
		ws.baud.SetText("19200")
		cfg, err = ws.connConfig()
	})
	if err != nil || cfg.serial.Parity != "E" || cfg.serial.StopBits != 1 || cfg.serial.DataBits != 8 || cfg.serial.BaudRate != 19200 {
		t.Errorf("串口参数 %+v %v", cfg.serial, err)
	}
}

// ASCII over TCP 全流程：内置模拟器按 ASCII 应答，读取窗口照常显示工程值，报文按字符显示并逐字段解析。
func TestASCIIEndToEnd(t *testing.T) {
	a := test.NewTempApp(t)
	ws := openWS(t, a, false)
	locked(func() {
		ws.proto.SetSelected(protoASCIITCP)
		ws.loadDemo()
	})
	waitFor(t, 5*time.Second, "ASCII 模式收到数据", func() bool { return hasData(ws.windows[0]) && hasData(ws.windows[1]) })
	var v1, v2 string
	var rx modbus.Packet
	locked(func() {
		v1, _ = ws.windows[0].valueText(0)
		v2, _ = ws.windows[1].valueText(0)
		ws.traffic.flush()
		for _, p := range ws.traffic.all {
			if p.Dir == modbus.DirRX && p.Status == modbus.StatusSuccess && p.Address == 346 {
				rx = p
			}
		}
	})
	if v1 != "45.2" || v2 != "15.0" {
		t.Errorf("ASCII 模式下的值 %q %q", v1, v2)
	}
	if line, _ := trafficLine(rx, false); !strings.Contains(line, "-:0103100000417") || !strings.Contains(line, " CRLF") {
		t.Errorf("ASCII 报文应按字符显示：%q", line)
	}
	text := rowsText(describePacket(rx, ws.points))
	for _, want := range []string{"起始符", "Slave ID = 1", "40347", "温差设定 15.0 ℃", "LRC", "校验正确", "CR LF"} {
		if !strings.Contains(text, want) {
			t.Errorf("ASCII 报文解析缺少 %q：\n%s", want, text)
		}
	}
	bad := rx
	bad.Raw = append([]byte(nil), rx.Raw...)
	bad.Raw[len(bad.Raw)-3] ^= 1
	if text := rowsText(describePacket(bad, nil)); !strings.Contains(text, "校验错误：收到") {
		t.Errorf("LRC 错误解析：\n%s", text)
	}
	if got := frameText(modbus.ModeASCII, []byte{0x01, 0x03}); got != "01 03" {
		t.Errorf("ASCII 模式下收到二进制应显示十六进制：%q", got)
	}
	locked(func() { ws.disconnect() })
}
