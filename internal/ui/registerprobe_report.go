package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

)

// exportReportDialog 先问实际设备备注（可空，只写进报告），再选保存位置，在后台写文件并核对。
func (ws *Workspace) exportReportDialog(r *probeRun, out probeOutput) {
	noteE := widget.NewEntry()
	noteE.SetPlaceHolder("例如 172.16.16.102:503，可不填")
	hint := widget.NewLabel("报告写明连接入口；实际设备地址只是你确认后的备注，不用于连接。同目录另存同名的 JSONL 明细和 CSV 失败清单。")
	hint.Wrapping = fyne.TextWrapWord
	form := dialog.NewForm("导出检测报告", "选择保存位置", "取消", []*widget.FormItem{
		widget.NewFormItem("实际设备（可选）", noteE), widget.NewFormItem("", hint),
	}, func(ok bool) {
		if !ok {
			return
		}
		note := strings.TrimSpace(noteE.Text)
		d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
			if err != nil || wc == nil {
				return
			}
			path := wc.URI().Path()
			scheme := wc.URI().Scheme()
			wc.Close()
			if scheme != "file" {
				dialog.ShowError(errors.New("检测报告只能保存到本机文件夹"), ws.win)
				return
			}
			path = filepath.FromSlash(path)
			ws.fileTask(ws.win, "导出检测报告", "正在写报告和明细："+filepath.Base(path), func(ctx context.Context) (func(), error) {
				files, err := exportProbeReport(ctx, r, out, note, path)
				if err != nil {
					return nil, fmt.Errorf("检测报告没有完整保存：%w", err)
				}
				return func() {
					var names []string
					for _, f := range files {
						names = append(names, filepath.Base(f))
					}
					dialog.ShowInformation("导出检测报告", fmt.Sprintf("已保存并核对 %d 个文件（JSONL %d 条请求，CSV %d 行）：\n%s",
						len(files), len(r.Log.Snapshot()), len(out.CSVRows), strings.Join(names, "\n")), ws.win)
				}, nil
			})
		}, ws.win)
		d.SetFileName(fmt.Sprintf("检测报告-Slave%d-FC%02d-%s.md", r.Def.Slave, byte(r.Def.Function), r.Started.Format("20060102-150405")))
		d.Show()
	}, ws.win)
	form.Resize(fyne.NewSize(520, 0))
	form.Show()
}
