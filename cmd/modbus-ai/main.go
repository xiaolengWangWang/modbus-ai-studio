// modbus-ai 是 Modbus AI Studio 桌面应用入口。
package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"modbus-ai-studio/internal/recorder"
	"modbus-ai-studio/internal/ui"
	"modbus-ai-studio/internal/update"
	"modbus-ai-studio/platform"
)

// version 在打包时用 -ldflags "-X main.version=…" 覆盖。
var version = "0.11.14"

func main() {
	start := time.Now()
	platform.Setup()
	update.Cleanup() // 删掉上次更新换下来的旧文件
	log.Printf("startup setup: %s", time.Since(start))
	go platform.PrewarmFonts()
	a := app.NewWithID("studio.modbusai.desktop")
	log.Printf("startup app: %s", time.Since(start))
	platform.ConfigureApp(a)
	log.Printf("startup theme: %s", time.Since(start))
	// 全部收发记录存进本机 SQLite；打不开时照常运行，只是不记录
	path, err := recorder.DefaultPath()
	var rec *recorder.Recorder
	if err == nil {
		rec, err = recorder.Open(path)
	}
	ui.SetRecorder(rec, path, err)
	log.Printf("startup recorder: %s", time.Since(start))
	if err != nil {
		fmt.Fprintln(os.Stderr, "报文记录不可用：", err)
	} else {
		defer rec.Close() // 退出前写完缓冲里的记录
	}
	desktop := ui.NewDesktop(a, version)
	defer desktop.Shutdown() // 先关掉全部连接，再由上面的 defer 写完数据库缓冲
	ws := desktop.Open()
	ws.AutoCheckUpdate()
	debugStart(ws)
	log.Printf("startup window shown: %s", time.Since(start))
	fyne.Do(func() { log.Printf("startup first UI event: %s", time.Since(start)) })
	a.Run()
	log.Printf("startup exit: %s", time.Since(start))
}
