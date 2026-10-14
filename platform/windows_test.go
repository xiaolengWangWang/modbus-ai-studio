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

// 候选中文字体里这台 Windows 有的，都能取出来给 Fyne 用：字族对、中文字形都在。
// 字体集合（msyh.ttc、simsun.ttc）要取对其中那个字体。
func TestCJKFontsFromSystem(t *testing.T) {
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	found := 0
	for _, f := range cjkFonts {
		for _, name := range []string{f.regular, f.bold} {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); name == "" || err != nil {
				continue
			}
			found++
			res, err := fontResource(path, f.face)
			if err != nil {
				t.Fatalf("%s：%v", name, err)
			}
			face, err := font.ParseTTF(bytes.NewReader(res.Content()))
			if err != nil {
				t.Fatalf("%s：Fyne 解析不了取出的字体：%v", name, err)
			}
			meta := face.Describe()
			if !strings.HasPrefix(strings.ToLower(meta.Family), strings.ToLower(f.name)) {
				t.Errorf("%s 取出的是 %q，应为 %s", name, meta.Family, f.name)
			}
			for _, r := range "中文字体 Modbus 40001" {
				if _, ok := face.NominalGlyph(r); !ok {
					t.Errorf("%s 缺少字形 %q", name, r)
				}
			}
			t.Logf("%s → %s（%d KB）", name, meta.Family, len(res.Content())>>10)
		}
	}
	if found == 0 {
		t.Skip("这台 Windows 一个候选中文字体都没有")
	}
}

// 兼容版把配置和缓存目录改到程序目录；启动别的程序时换回原来的环境变量。
func TestPortableDataDir(t *testing.T) {
	t.Setenv("APPDATA", `C:\original\Roaming`)
	t.Setenv("LOCALAPPDATA", `C:\original\Local`)
	t.Cleanup(func() { clear(envSaved) })
	dir := filepath.Join(t.TempDir(), "data")
	if err := portable(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.UserConfigDir(); got != dir {
		t.Errorf("配置目录 %s，应为 %s", got, dir)
	}
	if got, _ := os.UserCacheDir(); got != filepath.Join(dir, "cache") {
		t.Errorf("缓存目录 %s，应为 %s", got, filepath.Join(dir, "cache"))
	}
	env := strings.Join(ChildEnv(), "\n")
	for _, want := range []string{`APPDATA=C:\original\Roaming`, `LOCALAPPDATA=C:\original\Local`} {
		if !strings.Contains("\n"+env+"\n", "\n"+want+"\n") {
			t.Errorf("给别的程序的环境变量里没有 %s", want)
		}
	}
	if strings.Contains(env, dir) {
		t.Errorf("给别的程序的环境变量里不该有程序目录：%s", dir)
	}

	// 不能写的目录（这里用一个文件冒充）不改环境变量
	clear(envSaved)
	t.Setenv("APPDATA", `C:\original\Roaming`)
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	if err := portable(filepath.Join(file, "data")); err == nil {
		t.Error("目录建不了时应该报错")
	}
	if got := os.Getenv("APPDATA"); got != `C:\original\Roaming` || ChildEnv() != nil {
		t.Errorf("不能写时不该改环境变量：APPDATA=%s", got)
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
