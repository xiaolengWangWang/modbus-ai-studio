package main

import "bytes"

// glFailed 判断一行日志是不是“界面因为 OpenGL 起不来”：显卡不支持 OpenGL 2.1 时，
// Fyne 只用 log 写一行 “Fyne error:  window creation error”（或 GLFW 初始化失败）就退出。
func glFailed(line []byte) bool {
	return bytes.Contains(line, []byte("window creation error")) || bytes.Contains(line, []byte("failed to initialise GLFW"))
}

const glHelp = "界面启动失败：这台电脑的显卡不支持 OpenGL 2.1（远程桌面、虚拟机、老工控机常见）。\n\n" +
	"解决办法：下载 Mesa3D 软件渲染版的 opengl32.dll（https://github.com/pal1000/mesa-dist-win ，x64 目录），" +
	"放到 ModbusAIStudio.exe 同一目录，再重新打开。\n\n详细日志：%AppData%\\ModbusAIStudio\\app.log"
