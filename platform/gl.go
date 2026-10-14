// Package platform 放各操作系统专用的代码：Windows 的启动日志和 OpenGL 提示在 windows.go，
// macOS、Linux 目前没有专用代码（other.go）。各平台的打包配置在 macos/、windows/ 子目录。
package platform

import "bytes"

// glFailed 判断一行日志是不是“界面因为 OpenGL 起不来”：显卡不支持 OpenGL 2.1 时，
// Fyne 只用 log 写一行 “Fyne error:  window creation error”（或 GLFW 初始化失败）就退出。
func glFailed(line []byte) bool {
	return bytes.Contains(line, []byte("window creation error")) || bytes.Contains(line, []byte("failed to initialise GLFW"))
}

// glHelp 是普通版界面起不来时的说明：换用自带软件渲染的兼容版。page 是下载页面，弹窗问用户要不要打开。
func glHelp(logPath, page string) string {
	return "界面启动失败：这台电脑的显卡不支持 OpenGL 2.1，或显卡驱动有问题（远程桌面、虚拟机、老工控机常见）。\n\n" +
		"解决办法：下载兼容版（文件名以 -Windows-x64-Compat.zip 结尾），解压后运行里面的 ModbusAIStudio.exe。" +
		"兼容版自带软件渲染，不依赖显卡，也能在 Windows 7 上运行。\n\n" +
		"下载页面：" + page + "\n详细日志：" + logPath + "\n\n现在打开下载页面吗？"
}

// glHelpMesa 是已经用 Mesa 软件渲染还起不来时的说明。
func glHelpMesa(logPath string) string {
	return "界面启动失败：软件渲染（Mesa）也没能创建窗口。\n\n请把详细日志发给技术支持：" + logPath
}
