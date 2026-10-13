package web

import (
	"testing"

	"modbus-ai-studio/internal/modbus"
)

// 导入点表：相近的点合成读取表，替换原来的表；值和桌面版一样带枚举含义。批量改站号改全部读取表。
func TestImportPointsAndSetSlave(t *testing.T) {
	e := newEnv(t)
	e.login()
	if code := e.call("POST", "/api/demo", nil, nil); code != 200 {
		t.Fatalf("示例 %d", code)
	}
	table := "地址,名称,类型,字节序,倍率,单位,读写,最小,最大,枚举\n" +
		"40001,二次供水温度,FLOAT32,CDAB,1,℃,R,,,\n" +
		"40005,二次温差,INT16,,0.1,℃,R,,,\n" +
		"40353,运行模式,UINT16,,,,RW,,,0=手动;1=自动\n" +
		"40601,室外温度,FLOAT32,CDAB,1,℃,R,,,\n"
	var res importView
	if code := e.call("POST", "/api/points", importRequest{Name: "点表.csv", Data: []byte(table)}, &res); code != 200 || res.Points != 4 || res.Tables != 3 || res.Slave != 1 {
		t.Fatalf("导入 %d：%+v", code, res)
	}
	st := e.wait("按点表读出", func(st stateView) bool {
		r1, ok1 := rowByName(st, "运行模式")
		r2, ok2 := rowByName(st, "室外温度")
		return ok1 && ok2 && r1.Value != "" && r2.Value != ""
	})
	if len(st.Tables) != 3 {
		t.Fatalf("应替换示例的三张读取表：%d 张", len(st.Tables))
	}
	for name, want := range map[string]string{"运行模式": "1 自动", "室外温度": "-3.5", "二次温差": "13.2"} {
		if r, _ := rowByName(st, name); r.Value != want {
			t.Errorf("%s：%q，期望 %q", name, r.Value, want)
		}
	}
	if r, _ := rowByName(st, "运行模式"); !r.Writable || r.Type != "UINT16" {
		t.Errorf("可写点应能写，类型给写入框预填：%+v", r)
	}

	var out map[string]int
	if code := e.call("POST", "/api/slave", slaveRequest{To: 7}, &out); code != 200 || out["changed"] != 3 {
		t.Fatalf("改站号 %d：%v", code, out)
	}
	e.wait("全部读取表改用 Slave 7", func(st stateView) bool {
		for _, tv := range st.Tables {
			if tv.Config.Slave != 7 || len(tv.Config.Points) == 0 {
				return false
			}
		}
		return len(st.Tables) == 3
	})
	if code := e.call("POST", "/api/slave", slaveRequest{From: 7, To: 0}, nil); code != 400 {
		t.Errorf("站号 0 是广播，应拒绝：%d", code)
	}
	if code := e.call("POST", "/api/points", importRequest{Name: "a.csv", Data: []byte("地址,名称\n")}, nil); code != 400 {
		t.Errorf("不是点表的文件应拒绝：%d", code)
	}
}

// 事务号不是本连接发出的帧，报文里单独标出来，页面按“非本连接的响应”显示。
func TestForeignPacketFlag(t *testing.T) {
	var r packetRing
	r.add(modbus.Packet{Dir: modbus.DirRX, Status: modbus.StatusLate, Err: &modbus.ForeignError{TxID: 0x1CA6}})
	r.add(modbus.Packet{Dir: modbus.DirRX, Status: modbus.StatusLate, Err: &modbus.LateError{Timeout: 1}})
	got := r.since(0)
	if len(got) != 2 || !got[0].Foreign || got[1].Foreign {
		t.Fatalf("只有别的主站的帧标 foreign：%+v", got)
	}
}
