package ui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/transport"
)

type probeClient interface {
	Mode() modbus.Mode
	Do(context.Context, modbus.Request) (*modbus.Response, error)
}

var errProbeUnstable = errors.New("设备在读取该范围时反复断开连接，结果无法判断")

// networkProbeClient 独占检测连接，自行重连；错误不交给主会话的连接保持逻辑。
// 连接和客户端只由检测 goroutine 使用，取消回调只关闭当次请求的 socket。
type networkProbeClient struct {
	cfg      connConfig
	observer modbus.Observer
	waiting  func(bool)
	conn     net.Conn
	client   *modbus.Client
}

func (c *networkProbeClient) Mode() modbus.Mode { return c.cfg.mode }

func (c *networkProbeClient) close() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn, c.client = nil, nil
}

func (c *networkProbeClient) ready(ctx context.Context) error {
	if c.client != nil {
		return ctx.Err()
	}
	c.waiting(true)
	defer c.waiting(false)
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		t, err := transport.DialTCP(dialCtx, c.cfg.target, 3*time.Second)
		if err == nil {
			_, err = probeClosed(t, 100*time.Millisecond)
		}
		if dialCtx.Err() != nil {
			if t != nil {
				t.Close()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("检测连接重连超过 30 秒，已保留检测结果：%w", modbus.ErrConnection)
		}
		if err == nil {
			c.conn = t
			c.client = modbus.NewClient(t, modbus.Options{Mode: c.cfg.mode, Timeout: c.cfg.timeout, Observer: c.observer})
			return nil
		}
		if t != nil {
			t.Close()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-dialCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *networkProbeClient) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ready(ctx); err != nil {
			return nil, err
		}
		client := c.client
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			client.Close()
			close(closed)
		})
		response, err := client.Do(ctx, req)
		if !stop() {
			<-closed
		}
		if ctx.Err() != nil {
			c.close()
			return nil, ctx.Err()
		}
		if !errors.Is(err, modbus.ErrConnection) {
			return response, err
		}
		c.close()
	}
	// 同一个范围重连后仍断线：交给分段/逐点检测归类，继续后面的点。
	return nil, errProbeUnstable
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
				s.dialErr = dialErrText(err)
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
