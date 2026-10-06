//go:build capture

package main

import (
	"os"

	"modbus-ai-studio/internal/ui"
)

// 调试构建（go build -tags capture）：启动后按顺序操作界面，用真实的 OpenGL 渲染截图存到 CAPTURE_DIR，
// 然后退出。终端没有屏幕录制权限时，用它检查真实界面。剧本见 internal/ui/capture.go。
func debugStart(ws *ui.Workspace) { ws.CaptureScenario(os.Getenv("CAPTURE_DIR")) }
