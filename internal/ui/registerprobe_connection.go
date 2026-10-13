package ui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/probe"
	"modbus-ai-studio/internal/transport"
)

var errProbeUnstable = probe.ErrUnstable

// newNetworkProbeClient 是网络设备的独立检测连接（probe.NetClient）：独占连接，自行重连；
// 每次连上先看设备是不是马上关闭连接（连接数已满、IP 白名单），是就当没连上。
func newNetworkProbeClient(cfg connConfig, observer modbus.Observer, waiting func(bool)) *probe.NetClient {
	return &probe.NetClient{
		Options: modbus.Options{Mode: cfg.mode, Timeout: cfg.timeout, Observer: observer},
		Dial: func(ctx context.Context) (modbus.Transport, error) {
			t, err := transport.DialTCP(ctx, cfg.target, 3*time.Second)
			if err == nil {
				if _, err = probeClosed(t, 100*time.Millisecond); err != nil {
					t.Close()
					return nil, err
				}
			}
			return t, err
		},
		Waiting: waiting,
	}
}

func (ws *Workspace) probeConfig() (connConfig, error) {
	if ws.connecting {
		return connConfig{}, errors.New("正在建立常规连接，请连接完成后检测")
	}
	if s := ws.session; s != nil {
		return connConfig{mode: s.mode, target: s.target, timeout: ws.timeout}, nil
	}
	cfg, err := ws.connConfig()
	if err != nil {
		return cfg, err
	}
	if cfg.mode.Serial() || cfg.useSim {
		return cfg, errors.New("串口或内置模拟器请先连接，再检测寄存器")
	}
	return cfg, nil
}

// restoreProbeSession 保留主会话和故障记录；检测故障不会计入常规连接的断线次数。
func (ws *Workspace) restoreProbeSession(s *session) {
	if s == nil || ws.session != s || ws.closed {
		return
	}
	if s.lost == nil {
		s.lost = &lossEvent{at: time.Now(), kind: lossRandom, err: "检测结束，恢复常规连接"}
	}
	// 新客户端必须使用新 socket，避免事务编号从头开始时采纳晚到的检测响应。
	previousDone := s.reconnectDone
	ctx, cancel := context.WithCancel(s.ctx)
	s.reconnectCancel = cancel
	done := make(chan struct{})
	s.reconnectDone = done
	go func() {
		defer close(done)
		if previousDone != nil {
			select {
			case <-previousDone:
			case <-ctx.Done():
				return
			}
		}
		t, err := transport.DialTCP(ctx, s.target, 3*time.Second)
		applied := make(chan struct{})
		uiDo(func() {
			defer close(applied)
			if ctx.Err() != nil || ws.session != s || ws.closed {
				if t != nil {
					t.Close()
				}
				return
			}
			if err != nil {
				s.dialErr = transport.DialErrText(err)
				ws.startReconnect(s, 0)
			} else {
				s.link.Store(linkFor(t))
				s.client = modbus.NewClient(t, ws.clientOptions(s, ws.timeout))
				s.lost, s.dialErr, s.retryAt = nil, "", time.Time{}
				s.backoff = 0
				for _, w := range ws.windows {
					w.start()
				}
			}
			ws.refreshStatus()
		})
		<-applied
	}()
}

// sessionProbeClient 每次请求取得当前连接，避免重连后继续使用已关闭的客户端。
// 只重试读取；重复断线的点记为无法判断，后面的点仍可继续检测。
type sessionProbeClient struct {
	ws      *Workspace
	session *session
	waiting func(bool)
}

func (c *sessionProbeClient) Mode() modbus.Mode { return c.session.mode }

func (c *sessionProbeClient) ready(ctx context.Context, previous *modbus.Client) (*modbus.Client, error) {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	waiting := false
	defer func() {
		if waiting {
			c.waiting(false)
		}
	}()
	for {
		type state struct {
			client *modbus.Client
			err    error
		}
		ch := make(chan state, 1)
		uiDo(func() {
			s := c.session
			switch {
			case c.ws.closed || c.ws.session != s:
				ch <- state{err: context.Canceled}
			case s.lost != nil && s.mode.Serial():
				ch <- state{err: fmt.Errorf("串口已断开，请重新连接后检测：%w", modbus.ErrConnection)}
			case s.lost == nil && s.client != previous:
				ch <- state{client: s.client}
			default:
				ch <- state{}
			}
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("等待设备重连超过 30 秒，已保留检测结果，请设备恢复后重新检测：%w", modbus.ErrConnection)
		case s := <-ch:
			if s.err != nil || s.client != nil {
				return s.client, s.err
			}
		}
		if !waiting {
			waiting = true
			c.waiting(true)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return nil, fmt.Errorf("等待设备重连超过 30 秒，已保留检测结果：%w", modbus.ErrConnection)
		case <-timer.C:
		}
	}
}

func (c *sessionProbeClient) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
	client, err := c.ready(ctx, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(ctx, req)
	if !errors.Is(err, modbus.ErrConnection) || c.Mode().Serial() || ctx.Err() != nil {
		return response, err
	}
	client, err = c.ready(ctx, client)
	if err != nil {
		return nil, err
	}
	response, err = client.Do(ctx, req)
	if errors.Is(err, modbus.ErrConnection) && ctx.Err() == nil {
		return nil, errProbeUnstable
	}
	return response, err
}
