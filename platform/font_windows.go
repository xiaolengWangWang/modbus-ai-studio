//go:build windows

package platform

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
)

type windowsTheme struct {
	fyne.Theme
	regular, bold fyne.Resource
}

func (t windowsTheme) Font(style fyne.TextStyle) fyne.Resource {
	if style.Monospace || style.Symbol {
		return t.Theme.Font(style)
	}
	if style.Bold && t.bold != nil {
		return t.bold
	}
	return t.regular
}

func (t windowsTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 13
	case theme.SizeNameHeadingText:
		return 22
	case theme.SizeNameSubHeadingText:
		return 17
	case theme.SizeNameCaptionText:
		return 10
	}
	return t.Theme.Size(name)
}

// cjkFonts 是界面用的中文字体，按顺序找第一个有的：微软雅黑 UI（Win8 起在 msyh.ttc 里）、
// 微软雅黑（Win7 是 msyh.ttf）、等线（Win10 起）、黑体、宋体。bold 为空时粗体也用常规字体。
// face 是字体集合（.ttc）里要的字族名开头，小写；单个字体文件不用写。
var cjkFonts = []struct{ name, regular, bold, face string }{
	{"Microsoft YaHei UI", "msyh.ttc", "msyhbd.ttc", "microsoft yahei ui"},
	{"Microsoft YaHei", "msyh.ttf", "msyhbd.ttf", ""},
	{"DengXian", "Deng.ttf", "Dengb.ttf", ""},
	{"SimHei", "simhei.ttf", "", ""},
	{"SimSun", "simsun.ttc", "", "simsun"},
}

// ConfigureApp 选一个系统自带的中文字体做界面字体。都没有时用 Fyne 的默认字体，缺的字由 Fyne 从系统字体里补。
func ConfigureApp(app fyne.App) {
	fontDir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	for _, f := range cjkFonts {
		regular, err := fontResource(filepath.Join(fontDir, f.regular), f.face)
		if err != nil {
			log.Printf("startup font %s: %v", f.name, err)
			continue
		}
		var bold fyne.Resource
		if f.bold != "" {
			if bold, err = fontResource(filepath.Join(fontDir, f.bold), f.face); err != nil {
				log.Printf("startup font %s bold: %v", f.name, err)
			}
		}
		app.Settings().SetTheme(windowsTheme{Theme: theme.DefaultTheme(), regular: regular, bold: bold})
		log.Printf("startup font: %s", f.name)
		return
	}
}

// fontResource 读一个字体文件。字体集合（.ttc）里取字族名以 face 开头的那个，重排成单个字体。
func fontResource(path, face string) (fyne.Resource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("invalid font %s", path)
	}
	if string(data[:4]) != "ttcf" {
		return fyne.NewStaticResource(filepath.Base(path), data), nil
	}
	loaders, err := ot.NewLoaders(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var selected *ot.Loader
	for _, ld := range loaders {
		meta, _ := font.Describe(ld, nil)
		if strings.HasPrefix(strings.ToLower(meta.Family), face) {
			selected = ld
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("%s 里没有 %s", path, face)
	}
	standalone, err := standaloneFont(selected)
	if err != nil {
		return nil, err
	}
	return fyne.NewStaticResource(filepath.Base(path)+".ttf", standalone), nil
}
