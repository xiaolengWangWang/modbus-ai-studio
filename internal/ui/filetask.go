package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// fileTaskDelay 之内完成的读写不弹进度框，免得每次保存都闪一下。
const fileTaskDelay = 300 * time.Millisecond

// fileTask 在后台读写文件，界面不等它：超过 fileTaskDelay 还没完成才显示进度和“取消”。
// run 在后台执行，返回的 apply 在界面线程上执行；取消或主窗口关闭后丢弃结果，不调用 apply，
// run 返回的错误显示在 parent 上。取消只是不再等待，run 要在 ctx 取消后尽快返回，写文件时不能留下半截文件。
// finally 不论成功、失败还是取消，最后都在界面线程上调用。
func (ws *Workspace) fileTask(parent fyne.Window, title, text string, run func(ctx context.Context) (func(), error), finally ...func()) {
	ctx, cancel := context.WithCancel(context.Background())
	label := widget.NewLabel(text)
	label.Wrapping = fyne.TextWrapWord
	dlg := dialog.NewCustom(title, "取消", container.NewVBox(label, widget.NewProgressBarInfinite()), parent)
	dlg.Resize(fyne.NewSize(420, 0))
	dlg.SetOnClosed(cancel)
	finished, shown := false, false
	timer := time.AfterFunc(fileTaskDelay, func() {
		uiDo(func() {
			if !finished && ctx.Err() == nil && !ws.closed {
				shown = true
				dlg.Show()
			}
		})
	})
	go func() {
		apply, err := run(ctx)
		uiDo(func() {
			defer cancel()
			for _, f := range finally {
				defer f()
			}
			finished = true
			timer.Stop()
			cancelled := ctx.Err() != nil // 先判断：关掉进度框也会触发取消
			if shown {
				dlg.Hide()
			}
			if cancelled || ws.closed {
				return
			}
			if err != nil {
				dialog.ShowError(err, parent)
				return
			}
			if apply != nil {
				apply()
			}
		})
	}()
}

// writeAll 把 data 写进 w 并关闭；ctx 取消后关闭 w，打断还没完成的写入。
func writeAll(ctx context.Context, w io.WriteCloser, data []byte) error {
	stop := context.AfterFunc(ctx, func() { _ = w.Close() })
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if !stop() { // 已因取消关闭
		return ctx.Err()
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	return err
}

// writeFileAtomic 先写同目录下的临时文件再改名替换，写到一半出错或取消时原文件保持不变。
func writeFileAtomic(ctx context.Context, path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err = writeAll(ctx, f, data); err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存 %s 失败：%w", filepath.Base(path), err)
	}
	return nil
}

// readLimited 读完 rc（最多 limit 字节）后关闭；ctx 取消后关闭 rc，打断还没完成的读取。
func readLimited(ctx context.Context, rc io.ReadCloser, limit int64, what string) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s超过 %d MB，不是本程序能打开的文件", what, limit>>20)
	}
	return data, err
}
