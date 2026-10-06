package platform

import (
	"log"
	"time"

	"github.com/go-text/typesetting/fontscan"
)

// PrewarmFonts 提前建好 go-text 的全局系统字体索引。Fyne 画第一个字之前要扫一遍系统字体
// （有缓存时 macOS 约 0.1 s，Windows 第一次启动要好几秒），在后台先扫，和窗口初始化同时进行。
func PrewarmFonts() {
	start := time.Now()
	_, err := fontscan.SystemFonts(nil, "")
	log.Printf("startup font scan: %s, error=%v", time.Since(start), err)
}
