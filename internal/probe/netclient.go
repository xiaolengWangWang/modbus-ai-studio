package probe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"modbus-ai-studio/internal/modbus"
)

// NetClient 是网络设备的检测连接：独占一条连接，断线自己重连，错误不交给常规读取的连接保持逻辑。
// 只由检测 goroutine 使用，取消时只关闭当次请求的连接。桌面版和 modbus-cli 共用。
type NetClient struct {
	Options modbus.Options                                  // 协议、超时、收发回调
	Dial    func(context.Context) (modbus.Transport, error) // 建一条连接；断线后用它重连
	Waiting func(bool)                                      // 开始、结束等待重连，可以为空
	conn    modbus.Transport
	client  *modbus.Client
}

func (c *NetClient) Mode() modbus.Mode { return c.Options.Mode }

// Close 关闭当前连接；之后再发请求会重新建连。
func (c *NetClient) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn, c.client = nil, nil
}

func (c *NetClient) waiting(on bool) {
	if c.Waiting != nil {
		c.Waiting(on)
	}
}

// ready 确保有可用的连接：没有就每 100 ms 重连一次，最多等 30 秒。
func (c *NetClient) ready(ctx context.Context) error {
	if c.client != nil {
		return ctx.Err()
	}
	c.waiting(true)
	defer c.waiting(false)
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		t, err := c.Dial(dialCtx)
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
			c.client = modbus.NewClient(t, c.Options)
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

func (c *NetClient) Do(ctx context.Context, req modbus.Request) (*modbus.Response, error) {
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
			c.Close()
			return nil, ctx.Err()
		}
		if c.Mode() == modbus.ModeTCP && errors.Is(err, modbus.ErrTimeout) {
			// 设备可能仍在处理超时请求。关闭检测连接，下次请求重新建连，
			// 避免慢点的旧请求积压让后面的正常点也被误判为未响应。
			c.Close()
			return response, err
		}
		if !errors.Is(err, modbus.ErrConnection) {
			return response, err
		}
		c.Close()
	}
	// 同一个范围重连后仍断线：交给分段 / 逐点检测归类，继续后面的点。
	return nil, ErrUnstable
}
