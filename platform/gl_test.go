package platform

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
)

// 用 Fyne 自己的 LogError 生成日志，确认能认出 OpenGL 起不来的那一行，普通错误不误报。
func TestGLFailed(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fyne.LogError("window creation error", errors.New("WGL: The driver does not appear to support OpenGL"))
	if !glFailed(buf.Bytes()) {
		t.Errorf("没认出 OpenGL 失败：%q", buf.String())
	}
	buf.Reset()
	fyne.LogError("failed to open file", errors.New("permission denied"))
	if glFailed(buf.Bytes()) {
		t.Errorf("普通错误不该弹窗：%q", buf.String())
	}
}

// 普通版的说明指向兼容版和下载页面，已经用软件渲染时不再让用户去下兼容版；两种都给出实际的日志位置。
func TestGLHelp(t *testing.T) {
	text := glHelp(`D:\app\data\app.log`, "https://gitee.com/x/y/releases")
	for _, want := range []string{"-Windows-x64-Compat.zip", "https://gitee.com/x/y/releases", `D:\app\data\app.log`} {
		if !strings.Contains(text, want) {
			t.Errorf("说明里没有 %s：%s", want, text)
		}
	}
	text = glHelpMesa(`D:\app\data\app.log`)
	if !strings.Contains(text, `D:\app\data\app.log`) || strings.Contains(text, "Compat") {
		t.Errorf("软件渲染失败的说明不对：%s", text)
	}
}
