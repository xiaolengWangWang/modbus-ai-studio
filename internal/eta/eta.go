// Package eta 写长任务的进度说明：百分比、已用时间、预计还要多久。桌面版、Web 版和 modbus-cli 共用。
package eta

import (
	"fmt"
	"time"
)

// Text 是长任务的进度说明：百分比、已用时间、预计还要多久。刚开始估不准，先不报剩余时间。
func Text(frac float64, started time.Time) string {
	elapsed := time.Since(started)
	text := fmt.Sprintf("%d%% · 已用 %s", int(frac*100), Rough(elapsed))
	if frac >= 0.03 && frac < 1 && elapsed >= 500*time.Millisecond {
		text += " · 预计还要 " + Rough(time.Duration(float64(elapsed)*(1-frac)/frac))
	}
	return text
}

// Rough 把时长写成“3 秒”“2 分 5 秒”，进度说明不需要更细。
func Rough(d time.Duration) string {
	switch {
	case d < time.Second:
		return "不到 1 秒"
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Round(time.Second)/time.Second))
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%d 分 %d 秒", int(d/time.Minute), int(d%time.Minute/time.Second))
}
