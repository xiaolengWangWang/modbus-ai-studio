package slavescan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// bus 是假的总线：answer 按站号和读取给出结果，nil 表示正常响应；没有登记的读取都超时。
type bus struct {
	answer map[int]map[Probe]error
	gate   bool // 网关：不在线的站号回异常 0B，而不是超时
	sent   []string
	broken int // 发给这个站号时连接断开
	cancel func()
	stopAt int // 发给这个站号时取消
	retry  int // 没标“不重试”的请求数
}

func (b *bus) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
	p := Probe{req.Function, req.Address}
	b.sent = append(b.sent, fmt.Sprintf("%d %s", req.Slave, p))
	if !req.NoRetry {
		b.retry++
	}
	switch int(req.Slave) {
	case b.broken:
		return nil, fmt.Errorf("读的时候断开：%w", modbus.ErrConnection)
	case b.stopAt:
		b.cancel()
		return nil, ctx.Err()
	}
	if err, ok := b.answer[int(req.Slave)][p]; ok {
		if err != nil {
			return nil, err
		}
		return &modbus.Response{}, nil
	}
	if b.gate {
		return nil, &modbus.ExceptionError{Function: req.Function, Code: modbus.ExceptionGatewayTargetFailed}
	}
	return nil, modbus.ErrTimeout
}

var (
	reg40001 = Probe{modbus.FuncReadHoldingRegisters, 0}
	reg30001 = Probe{modbus.FuncReadInputRegisters, 0}
	coil1    = Probe{modbus.FuncReadCoils, 0}
	reg40101 = Probe{modbus.FuncReadHoldingRegisters, 100}
)

func ids(hits []Hit) string {
	var s []string
	for _, h := range hits {
		s = append(s, fmt.Sprintf("%d@%s", h.ID, h.Probe))
	}
	return strings.Join(s, " ")
}

// 读 40001 不应答的设备换别的读取还能找到；异常响应、89 回显也算在线；没有设备的站号每种读取都试过。
func TestScanTriesOtherReads(t *testing.T) {
	b := &bus{answer: map[int]map[Probe]error{
		2: {reg40001: nil},
		3: {reg40001: &modbus.ExceptionError{Function: modbus.FuncReadHoldingRegisters, Code: modbus.ExceptionIllegalDataAddress}},
		4: {coil1: nil}, // 40001、30001 都不应答，只有线圈回
		5: {reg40001: &modbus.NonstandardError{Function: modbus.FuncReadHoldingRegisters, PDU: []byte{0x89, 0, 0, 0, 1}}},
	}}
	var done []int
	hits, err := Scan(context.Background(), b, 1, 6, 50*time.Millisecond, Probes(), func(id int, _ []Hit) { done = append(done, id) })
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hits); got != "2@40001（FC03） 3@40001（FC03） 4@线圈 1（FC01） 5@40001（FC03）" {
		t.Fatalf("找到 %s", got)
	}
	if b.retry != 0 {
		t.Errorf("%d 个请求没标不重试：没设备的站号会多等一倍", b.retry)
	}
	if len(done) != 6 || len(b.sent) != 3+1+1+3+1+3 {
		t.Errorf("进度 %v，发了 %d 次：%v", done, len(b.sent), b.sent)
	}
	for i, want := range []string{"正常响应", "异常 02 非法数据地址", "线圈 1（FC01） 正常响应", "89 回显"} {
		if !strings.Contains(hits[i].Note(), want) {
			t.Errorf("Slave %d 的说明 %q 里没有 %q", hits[i].ID, hits[i].Note(), want)
		}
	}
}

// 网关替不在线的站号回的异常 0B 不算找到，继续换读取；用户给的地址先试。
func TestScanThroughGateway(t *testing.T) {
	b := &bus{gate: true, answer: map[int]map[Probe]error{7: {reg40101: nil}}}
	hits, err := Scan(context.Background(), b, 6, 8, 50*time.Millisecond, Probes(reg40101), nil)
	if err != nil || ids(hits) != "7@40101（FC03）" {
		t.Fatalf("找到 %s（%v）", ids(hits), err)
	}
	if b.sent[0] != "6 40101（FC03）" || len(b.sent) != 4+1+4 {
		t.Errorf("应先试用户给的地址、没设备的站号四种都试：%v", b.sent)
	}
}

// 连接断开时停下，返回已找到的和错误；取消时返回已找到的，不算出错。
func TestScanStops(t *testing.T) {
	b := &bus{broken: 4, answer: map[int]map[Probe]error{2: {reg40001: nil}}}
	hits, err := Scan(context.Background(), b, 1, 10, time.Millisecond, Probes(), nil)
	if !errors.Is(err, modbus.ErrConnection) || ids(hits) != "2@40001（FC03）" {
		t.Fatalf("断开：%s %v", ids(hits), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b = &bus{stopAt: 3, cancel: cancel, answer: map[int]map[Probe]error{1: {reg30001: nil}}}
	hits, err = Scan(ctx, b, 1, 10, time.Millisecond, Probes(), nil)
	if err != nil || ids(hits) != "1@30001（FC04）" || len(b.sent) != 2+3+1 {
		t.Fatalf("取消：%s %v，发了 %v", ids(hits), err, b.sent)
	}
}

func TestParseProbe(t *testing.T) {
	for in, want := range map[string]Probe{
		"40101":  reg40101,
		"30005":  {modbus.FuncReadInputRegisters, 4},
		"346":    {modbus.FuncReadHoldingRegisters, 346},
		"10003":  {modbus.FuncReadDiscreteInputs, 2},
		"0x0064": {modbus.FuncReadHoldingRegisters, 100},
	} {
		if got, err := ParseProbe(in); err != nil || got != want {
			t.Errorf("%s → %v %v，应为 %v", in, got, err, want)
		}
	}
	if _, err := ParseProbe("abc"); err == nil {
		t.Error("abc 应报错")
	}
	if got := Probes(reg40001, reg40101); len(got) != 4 || got[0] != reg40001 || got[1] != reg40101 {
		t.Errorf("去重后 %v", got)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		from, to int
		tmo      time.Duration
		ok       bool
	}{{1, 247, 200 * time.Millisecond, true}, {0, 5, time.Second, false}, {5, 4, time.Second, false}, {1, 256, time.Second, false},
		{1, 2, 10 * time.Millisecond, false}, {1, 2, 11 * time.Second, false}} {
		if err := Validate(c.from, c.to, c.tmo); (err == nil) != c.ok {
			t.Errorf("%d–%d %v：%v", c.from, c.to, c.tmo, err)
		}
	}
	if w := Worst(1, 10, 100*time.Millisecond, 4); w != 4*time.Second {
		t.Errorf("最坏 %v", w)
	}
}
