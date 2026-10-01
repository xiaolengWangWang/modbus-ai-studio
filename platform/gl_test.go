package platform

import (
	"bytes"
	"errors"
	"log"
	"os"
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
