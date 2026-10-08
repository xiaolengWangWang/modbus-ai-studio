package ui

import (
	"bytes"
	"encoding/json"
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

func aiEvidenceName(kind string) string {
	labels := map[string]string{
		"workspace_state": "连接状态", "current_read_definition": "读取参数", "current_read_state": "读取状态", "current_registers": "寄存器值",
		"selected_packet": "选中报文", "related_packet": "关联报文", "packet": "近期报文", "selected_log": "选中日志", "log_tx": "请求信息", "log_result": "响应信息", "register_probe": "检测结果",
	}
	if label := labels[kind]; label != "" {
		return label
	}
	return kind
}

func (t *aiTool) buildEvidenceBrowser() fyne.CanvasObject {
	t.evidenceDetail = widget.NewMultiLineEntry()
	t.evidenceDetail.TextStyle.Monospace = true
	t.evidenceDetail.Wrapping = fyne.TextWrapBreak
	t.evidenceDetail.Disable()
	t.copyEvidence = widget.NewButton("复制此条证据", func() { t.ws.app.Clipboard().SetContent(t.evidenceDetail.Text) })
	t.copyEvidence.Disable()
	t.evidenceList = widget.NewList(func() int { return len(t.snapshot.Evidence) }, func() fyne.CanvasObject {
		l := widget.NewLabel("")
		l.Truncation = fyne.TextTruncateEllipsis
		return l
	}, func(i widget.ListItemID, o fyne.CanvasObject) {
		e := t.snapshot.Evidence[i]
		o.(*widget.Label).SetText(e.ID + " · " + aiEvidenceName(e.Kind))
	})
	t.evidenceList.OnSelected = func(i widget.ListItemID) {
		if i < 0 || i >= len(t.snapshot.Evidence) {
			return
		}
		e := t.snapshot.Evidence[i]
		var b bytes.Buffer
		if err := json.Indent(&b, e.Data, "", "  "); err != nil {
			return
		}
		t.evidenceDetail.SetText(fmt.Sprintf("%s · %s\n\n%s", e.ID, aiEvidenceName(e.Kind), b.String()))
		t.copyEvidence.Enable()
	}
	split := container.NewHSplit(t.evidenceList, container.NewBorder(nil, t.copyEvidence, nil, nil, dense(t.evidenceDetail)))
	split.Offset = .28
	t.evidencePage = container.NewTabItem("证据列表", split)
	t.evidencePages = container.NewAppTabs(t.evidencePage, container.NewTabItem("完整 JSON", dense(t.preview)))
	return t.evidencePages
}

func (t *aiTool) updateEvidenceBrowser() {
	t.evidenceList.UnselectAll()
	t.evidenceList.Refresh()
	t.evidenceDetail.SetText("")
	t.copyEvidence.Disable()
	if len(t.snapshot.Evidence) > 0 {
		t.evidenceList.Select(0)
	}
}

func (t *aiTool) showEvidence(id string) {
	for i, e := range t.snapshot.Evidence {
		if e.ID == id {
			t.tabs.Select(t.previewTab)
			t.evidencePages.Select(t.evidencePage)
			t.evidenceList.Select(i)
			return
		}
	}
}
