package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/update"
)

// 软件更新：“帮助 → 检查更新”，以及启动几秒后自动检查（每天最多一次，可在“帮助”菜单里关掉）。
// 下载、校验、替换程序在 internal/update。

const (
	prefAutoUpdate = "update.auto"      // 自动检查更新，默认开
	prefLastCheck  = "update.lastCheck" // 上次成功检查的 Unix 秒
	prefSkip       = "update.skip"      // 用户选择跳过的版本，自动检查时不再提示
)

// 测试时替换：不写用户的缓存目录，也不真的替换测试程序。
var (
	updateCacheDir   = update.CacheDir
	installUpdatePkg = update.Install
)

// updating 表示正在检查或下载更新。多个主窗口共用，同一时间只做一次。
var updating atomic.Bool

func autoUpdate(app fyne.App) bool { return app.Preferences().BoolWithFallback(prefAutoUpdate, true) }

// AutoCheckUpdate 启动后自动检查一次更新：每天最多一次，用户关掉了就不查，查不到也不打扰。
// 由 main 在打开第一个主窗口后调用；测试不调用，不访问网络。
func (ws *Workspace) AutoCheckUpdate() {
	last := time.Unix(int64(ws.app.Preferences().Int(prefLastCheck)), 0)
	if !autoUpdate(ws.app) || time.Since(last) < 20*time.Hour {
		return
	}
	go func() {
		time.Sleep(3 * time.Second) // 先让界面起来
		uiDo(func() { ws.checkUpdate(false) })
	}()
}

// checkUpdate 查询最新版本。manual 为 true 时（菜单里点的）显示查询进度、已是最新、查询失败；
// 自动检查只在有新版本时弹窗。
func (ws *Workspace) checkUpdate(manual bool) {
	if ws.closed {
		return
	}
	if !updating.CompareAndSwap(false, true) {
		if manual {
			dialog.ShowInformation("检查更新", "正在检查或下载更新，下载进度见状态栏。", ws.win)
		}
		return
	}
	var wait *dialog.ProgressInfiniteDialog
	if manual {
		wait = dialog.NewProgressInfinite("检查更新", "正在从 GitHub 查询最新版本…", ws.win)
		wait.Show()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		rel, err := update.Latest(ctx)
		uiDo(func() {
			if wait != nil {
				wait.Hide()
			}
			if err == nil {
				ws.app.Preferences().SetInt(prefLastCheck, int(time.Now().Unix()))
			}
			switch {
			case ws.closed:
				updating.Store(false)
			case err != nil:
				updating.Store(false)
				if manual {
					ws.showUpdateError("检查更新失败", err)
				}
			case !update.Newer(rel.Version(), ws.Version):
				updating.Store(false)
				if manual {
					dialog.ShowInformation("检查更新", fmt.Sprintf("已是最新版本 %s。", ws.Version), ws.win)
				}
			case !manual && ws.app.Preferences().String(prefSkip) == rel.Version():
				updating.Store(false)
			case !manual && ws.win.Canvas().Overlays().Top() != nil:
				updating.Store(false) // 正开着别的对话框，不叠上去，下次启动再提示
			default:
				ws.showUpdate(rel, manual)
			}
		})
	}()
}

// showUpdate 说明新版本的内容，让用户选择下载安装、手动下载或以后再说。
func (ws *Workspace) showUpdate(rel update.Release, manual bool) {
	asset, hasAsset := rel.Asset(runtime.GOOS, runtime.GOARCH)
	sum, hasSum := rel.Checksum(asset.Name)
	canInstall := hasAsset && hasSum && update.CanInstall()
	var how string
	switch {
	case !hasAsset:
		how = "这个版本没有适合本机的安装包，请到下载页面查看。"
	case !hasSum:
		how = "发布说明里没有安装包的 SHA-256 校验值，为安全起见请到下载页面手动下载。"
	case !canInstall:
		how = "当前程序不是从安装包运行的（例如从源码运行），请到下载页面手动下载。"
	default:
		how = fmt.Sprintf("将下载 %s（%.1f MB），校验 SHA-256 后替换当前程序，然后重启。", asset.Name, float64(asset.Size)/(1<<20))
	}
	head := widget.NewLabel(fmt.Sprintf("发现新版本 %s（当前 %s）。\n%s", rel.Version(), ws.Version, how))
	head.Wrapping = fyne.TextWrapWord
	head.Resize(fyne.NewSize(560, 0)) // 自动换行的标签要先定宽度，MinSize 才是换行后的高度
	notes := widget.NewRichTextFromMarkdown(rel.Notes())
	notes.Wrapping = fyne.TextWrapWord
	scroll := container.NewVScroll(notes)
	scroll.SetMinSize(fyne.NewSize(560, 280))
	d := dialog.NewCustomWithoutButtons("软件更新", container.NewBorder(head, nil, nil, nil, scroll), ws.win)

	installing := false
	d.SetOnClosed(func() {
		if !installing {
			updating.Store(false)
		}
	})
	var buttons []fyne.CanvasObject
	if canInstall {
		b := widget.NewButton("下载并安装", func() {
			installing = true
			d.Hide()
			ws.installUpdate(rel, asset, sum)
		})
		b.Importance = widget.HighImportance
		buttons = append(buttons, b)
	}
	buttons = append(buttons, widget.NewButton("打开下载页面", func() { ws.openReleasePage(); d.Hide() }))
	if !manual {
		buttons = append(buttons, widget.NewButton("跳过这个版本", func() {
			ws.app.Preferences().SetString(prefSkip, rel.Version())
			d.Hide()
		}))
	}
	buttons = append(buttons, widget.NewButton("以后再说", d.Hide))
	d.SetButtons(buttons)
	d.Show()
}

// updateNote 是后台下载更新时状态栏显示的进度，所有主窗口共用；为空表示没有在下载。
var updateNote atomic.Pointer[string]

func setUpdateNote(s string) { updateNote.Store(&s) }

// updateStatus 是状态栏末尾的更新进度。
func updateStatus() string {
	if p := updateNote.Load(); p != nil && *p != "" {
		return " · " + *p
	}
	return ""
}

// installUpdate 下载、校验、替换程序。下载可以取消，也可以转到后台（状态栏显示进度）；
// 下了一半的文件留在缓存目录，下次接着下。装好后问是否立即重启。
func (ws *Workspace) installUpdate(rel update.Release, asset update.Asset, sum string) {
	ctx, cancel := context.WithCancel(context.Background())
	bar := widget.NewProgressBar()
	info := widget.NewLabel("正在连接…")
	tip := widget.NewLabel("网络慢时可以点“后台下载”继续用程序，下好后会提示安装；断开后会自动接着下。")
	tip.Wrapping = fyne.TextWrapWord
	tip.Resize(fyne.NewSize(420, 0))
	box := container.NewVBox(info, bar, tip)
	d := dialog.NewCustomWithoutButtons("下载新版本 "+rel.Version(), container.NewGridWrap(fyne.NewSize(420, box.MinSize().Height), box), ws.win)
	cancelBtn := widget.NewButton("取消", cancel)
	bgBtn := widget.NewButton("后台下载", d.Hide)
	d.SetButtons([]fyne.CanvasObject{bgBtn, cancelBtn})
	d.Show()
	start := time.Now()
	go func() {
		defer cancel()
		dir := updateCacheDir()
		pkg, err := update.Download(ctx, asset, sum, dir, func(done, total int64) {
			uiDo(func() {
				mb := float64(done) / (1 << 20)
				speed := mb / max(time.Since(start).Seconds(), 0.1)
				if total > 0 {
					bar.SetValue(float64(done) / float64(total))
					info.SetText(fmt.Sprintf("已下载 %.1f / %.1f MB · %.2f MB/s", mb, float64(total)/(1<<20), speed))
					setUpdateNote(fmt.Sprintf("正在下载新版本 %s：%.0f%%", rel.Version(), 100*float64(done)/float64(total)))
				} else {
					info.SetText(fmt.Sprintf("已下载 %.1f MB · %.2f MB/s", mb, speed))
					setUpdateNote(fmt.Sprintf("正在下载新版本 %s：%.1f MB", rel.Version(), mb))
				}
			})
		})
		var target string
		if err == nil {
			uiDo(func() {
				info.SetText("校验通过，正在替换程序…")
				setUpdateNote("正在安装新版本 " + rel.Version())
				cancelBtn.Disable()
				bgBtn.Disable()
			})
			target, err = installUpdatePkg(pkg)
			if err == nil {
				os.RemoveAll(dir) // 装好了，下载的安装包不再需要
			}
		}
		uiDo(func() {
			d.Hide()
			setUpdateNote("")
			updating.Store(false)
			switch {
			case ws.closed:
			case errors.Is(err, context.Canceled):
				// 用户取消；下了一半的文件留着，下次接着下
			case err != nil:
				ws.showUpdateError("更新失败", err)
			default:
				ws.askRestart(rel.Version(), target)
			}
		})
	}()
}

func (ws *Workspace) askRestart(version, target string) {
	dialog.ShowConfirm("更新完成", fmt.Sprintf("已更新到 %s。现在重启程序吗？\n所有主窗口都会关闭，没保存的工作区请先保存。", version), func(ok bool) {
		if !ok {
			dialog.ShowInformation("更新完成", "下次打开程序时就是新版本。", ws.win)
			return
		}
		if err := update.Restart(target); err != nil {
			ws.showUpdateError("重启失败", fmt.Errorf("%w。新版本已经装好，请手动打开程序", err))
			return
		}
		ws.app.Quit()
	}, ws.win)
}

// showUpdateError 说明更新失败的原因，并给出手动下载的入口。
func (ws *Workspace) showUpdateError(title string, err error) {
	l := widget.NewLabel(err.Error() + "\n\n可以打开下载页面手动下载安装包。")
	l.Wrapping = fyne.TextWrapWord
	l.Resize(fyne.NewSize(420, 0))
	d := dialog.NewCustomWithoutButtons(title, container.NewGridWrap(fyne.NewSize(420, l.MinSize().Height), l), ws.win)
	d.SetButtons([]fyne.CanvasObject{
		widget.NewButton("打开下载页面", func() { ws.openReleasePage(); d.Hide() }),
		widget.NewButton("关闭", d.Hide),
	})
	d.Show()
}

func (ws *Workspace) openReleasePage() {
	if u, err := url.Parse(update.PageURL); err == nil {
		ws.app.OpenURL(u)
	}
}

// toggleAutoUpdate 切换“自动检查更新”，菜单项的勾选随之更新。
func (ws *Workspace) toggleAutoUpdate() {
	on := !autoUpdate(ws.app)
	ws.app.Preferences().SetBool(prefAutoUpdate, on)
	ws.autoUpdItem.Checked = on
	if m := ws.win.MainMenu(); m != nil {
		m.Refresh()
	}
}
