//go:build windows

package platform

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-text/typesetting/font"
)

// 从 Windows 自带的 msyh.ttc / msyhbd.ttc 取出 Microsoft YaHei UI，Fyne 能解析，中文字形都在。
func TestYaHeiFromSystem(t *testing.T) {
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	for _, name := range []string{"msyh.ttc", "msyhbd.ttc"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Skipf("这台 Windows 没有 %s", name)
		}
		res, err := yaheiResource(path)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		face, err := font.ParseTTF(bytes.NewReader(res.Content()))
		if err != nil {
			t.Fatalf("%s：Fyne 解析不了取出的字体：%v", name, err)
		}
		meta := face.Describe()
		if !strings.Contains(strings.ToLower(meta.Family), "yahei ui") {
			t.Errorf("%s 取出的是 %q，应为 Microsoft YaHei UI", name, meta.Family)
		}
		for _, r := range "中文字体 Modbus 40001" {
			if _, ok := face.NominalGlyph(r); !ok {
				t.Errorf("%s 缺少字形 %q", name, r)
			}
		}
		t.Logf("%s → %s（%d KB）", name, meta.Family, len(res.Content())>>10)
	}
}

// 能取到屏幕可用区域，窗口按它限制大小。
func TestWorkAreaOnWindows(t *testing.T) {
	w, h := WorkArea()
	t.Logf("可用区域 %d × %d", w, h)
	if w <= 0 || h <= 0 {
		t.Errorf("取不到可用区域：%d × %d", w, h)
	}
}
