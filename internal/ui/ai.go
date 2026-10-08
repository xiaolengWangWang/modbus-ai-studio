package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"modbus-ai-studio/internal/ai"
	"modbus-ai-studio/internal/modbus"
)

type aiTool struct {
	ws                                                                  *Workspace
	win                                                                 fyne.Window
	tabs                                                                *container.AppTabs
	previewTab, settingsTab                                             *container.TabItem
	key, model, question, preview                                       *widget.Entry
	reportView                                                          *widget.RichText
	reportText, exportText                                              string
	includeRaw, includeValues, remember                                 *widget.Check
	send, cancelBtn, testBtn, refresh, save, forget, copyBtn, exportBtn *widget.Button
	state, destination, source, captureInfo                             *widget.Label
	snapshot                                                            ai.Snapshot
	target                                                              aiTarget
	origin                                                              *inspector
	endpoint                                                            string
	keyPath                                                             string
	busy, closed                                                        bool
	testOnly, invalidSource                                             bool
	keyLoadErr                                                          string
	generation                                                          uint64
	cancel                                                              context.CancelFunc
	refreshEvidence                                                     *widget.Button
	evidenceList                                                        *widget.List
	evidenceDetail                                                      *widget.Entry
	evidencePages                                                       *container.AppTabs
	evidencePage                                                        *container.TabItem
	copyEvidence                                                        *widget.Button
	quickQuestion                                                       *widget.Select
	progress                                                            *widget.ProgressBarInfinite
}

func cloneAIPacket(p *modbus.Packet) *modbus.Packet {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Raw = append([]byte(nil), p.Raw...)
	return &cp
}
func (ws *Workspace) openAI(p *modbus.Packet) {
	target := aiTarget{packet: p}
	if p == nil {
		target.read = ws.current()
	}
	ws.openAITarget(target)
}

func (ws *Workspace) openAITarget(target aiTarget) {
	ws.openAIFrom(target, ws.inspect)
}

func (ws *Workspace) openAIFrom(target aiTarget, origin *inspector) {
	target = ws.freezeAITarget(target, origin)
	if ws.closed {
		return
	}
	if t := ws.ai; t != nil {
		if t.busy {
			t.cancelRequest()
		}
		t.target, t.origin = target, origin
		if target.probe != nil {
			t.question.SetText("请解释这份寄存器检测结果，区分非法地址、未响应、无法判断和未检测，并给出下一步人工检查。")
		}
		t.capture()
		t.win.Show()
		t.win.RequestFocus()
		return
	}
	t := &aiTool{ws: ws, win: ws.app.NewWindow(fmt.Sprintf("AI 诊断助手 · 窗口 %d", ws.no)), target: target, origin: origin, endpoint: ai.Endpoint}
	ws.ai = t
	t.win.Resize(fyne.NewSize(800, 600))
	t.win.SetContent(t.build())
	t.win.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyReturn, Modifier: fyne.KeyModifierShortcutDefault}, func(fyne.Shortcut) {
		if len(t.win.Canvas().Overlays().List()) == 0 {
			t.start(false)
		}
	})
	ws.addTool(t.win, func() { t.closed = true; t.cancelRequest(); t.key.SetText(""); ws.ai = nil })
	t.capture()
	showTool(t.win)
}

func (t *aiTool) build() fyne.CanvasObject {
	t.key = widget.NewPasswordEntry()
	t.key.SetPlaceHolder("DeepSeek API Key")
	t.model = widget.NewEntry()
	t.model.SetText(t.ws.app.Preferences().StringWithFallback("ai.deepseek.model", ai.DefaultModel))
	t.keyPath, _ = ai.KeyPath()
	t.loadKey()
	t.key.OnChanged = func(key string) {
		if strings.TrimSpace(key) != "" {
			t.keyLoadErr = ""
		}
	}
	t.remember = widget.NewCheck("使用 Windows 当前用户加密保存 API Key", nil)
	t.remember.SetChecked(ai.ProtectedStorage)
	if !ai.ProtectedStorage {
		t.remember.SetText("此平台的 API Key 仅用于当前会话")
		t.remember.Disable()
	}
	t.state = widget.NewLabel("准备就绪，请先查看发送内容，再开始分析")
	t.state.Wrapping = fyne.TextWrapWord
	t.destination = widget.NewLabel("DeepSeek · api.deepseek.com · " + t.model.Text)
	t.destination.Truncation = fyne.TextTruncateEllipsis
	t.model.OnChanged = func(s string) { t.destination.SetText("DeepSeek · api.deepseek.com · " + s) }
	t.source = widget.NewLabel("")
	t.source.TextStyle.Bold = true
	t.source.Truncation = fyne.TextTruncateEllipsis
	t.captureInfo = widget.NewLabel("")
	t.captureInfo.Truncation = fyne.TextTruncateEllipsis
	t.preview = widget.NewMultiLineEntry()
	t.preview.TextStyle.Monospace = true
	t.preview.Wrapping = fyne.TextWrapBreak
	t.preview.Disable()
	t.reportView = widget.NewRichText()
	t.reportView.Wrapping = fyne.TextWrapWord
	t.setOutput("AI 根据冻结证据提供解释和人工检查建议。先在“模型设置”中测试连接，再查看“发送内容”。模型的原因判断需要现场核实。")
	t.question = widget.NewMultiLineEntry()
	t.question.SetMinRowsVisible(2)
	t.question.SetPlaceHolder("描述问题；Ctrl / ⌘ + Enter 开始分析，Enter 换行")
	t.question.SetText("请分析这份 Modbus 证据中的问题，说明事实、可能原因和下一步检查。")
	if t.target.probe != nil {
		t.question.SetText("请解释这份寄存器检测结果，区分非法地址、未响应、无法判断和未检测，并给出下一步人工检查。")
	}
	t.send = widget.NewButtonWithIcon("开始分析", theme.SearchIcon(), func() { t.start(false) })
	t.send.Importance = widget.HighImportance
	t.cancelBtn = widget.NewButton("取消", t.cancelRequest)
	t.cancelBtn.Disable()
	t.refresh = widget.NewButton("使用当前选择", func() {
		origin := t.origin
		if origin == nil {
			origin = t.ws.inspect
		}
		target := origin.aiTarget()
		t.target = t.ws.freezeAITarget(target, origin)
		t.origin = origin
		t.capture()
	})
	t.refreshEvidence = widget.NewButton("更新当前证据", func() {
		t.target = t.ws.freezeAITarget(t.target, t.origin)
		t.capture()
	})
	t.includeRaw = widget.NewCheck("发送原始报文（可能含过程值）", func(bool) { t.capture() })
	t.includeValues = widget.NewCheck("发送当前寄存器值", func(bool) { t.capture() })
	t.testBtn = widget.NewButton("测试 DeepSeek 连接", func() { t.start(true) })
	t.save = widget.NewButton("保存设置", t.saveSettings)
	t.forget = widget.NewButton("删除已保存密钥", func() {
		if t.keyPath != "" {
			if err := ai.DeleteKey(t.keyPath); err != nil {
				dialog.ShowError(err, t.win)
				return
			}
		}
		t.key.SetText("")
		t.keyLoadErr = ""
		t.state.SetText("加密凭据已删除；本窗口密钥已清空")
	})
	settingsNote := widget.NewLabel("请求仅发送到 https://api.deepseek.com；连接测试仅发送合成证据。\n勾选加密保存并保存设置后，下次启动可直接使用。取消勾选并保存会删除旧凭据。\n密钥不会保存到工作区、报文数据库或分析报告。也可使用 DEEPSEEK_API_KEY 环境变量。")
	settingsNote.Wrapping = fyne.TextWrapWord
	settings := newWrappingScroll(container.New(toolbarLayout{}, widget.NewForm(widget.NewFormItem("API Key", t.key), widget.NewFormItem("模型", t.model)), t.remember, container.New(flowLayout{}, t.save, t.forget, t.testBtn), settingsNote))
	resultTop := widget.NewLabel("点击 E 编号查看对应证据；检查步骤由你手动执行。")
	resultTop.Wrapping = fyne.TextWrapWord
	results := container.NewBorder(resultTop, nil, nil, nil, container.NewVScroll(t.reportView))
	previewTop := container.NewVBox(t.captureInfo, t.includeRaw, t.includeValues)
	previewContent := container.NewBorder(previewTop, nil, nil, nil, t.buildEvidenceBrowser())
	resultTab := container.NewTabItem("分析结果", results)
	t.previewTab = container.NewTabItem("发送内容", previewContent)
	t.settingsTab = container.NewTabItem("模型设置", settings)
	t.tabs = container.NewAppTabs(resultTab, t.previewTab, t.settingsTab)
	settingsBtn := widget.NewButton("模型设置", func() { t.tabs.Select(t.settingsTab) })
	previewBtn := widget.NewButton("查看发送内容", func() { t.tabs.Select(t.previewTab) })
	t.copyBtn = widget.NewButtonWithIcon("复制报告", theme.ContentCopyIcon(), func() { t.ws.app.Clipboard().SetContent(t.reportText) })
	t.copyBtn.Disable()
	t.copyBtn.Importance = widget.LowImportance
	t.exportBtn = widget.NewButtonWithIcon("导出报告", theme.DocumentSaveIcon(), t.exportReport)
	t.exportBtn.Disable()
	t.exportBtn.Importance = widget.LowImportance
	header := container.New(toolbarLayout{}, t.destination, t.source, container.New(flowLayout{}, t.refreshEvidence, t.refresh, previewBtn, settingsBtn, t.copyBtn, t.exportBtn))
	t.quickQuestion = widget.NewSelect([]string{"诊断当前问题", "建议检查步骤", "说明缺少的证据"}, func(s string) {
		questions := map[string]string{"诊断当前问题": "请根据当前证据区分事实与可能原因，说明诊断局限。", "建议检查步骤": "请给出最多三项人工检查，每项说明预期结果及其含义。", "说明缺少的证据": "目前还缺少哪些证据才能确认原因？请说明如何人工获取和核对。"}
		if q := questions[s]; q != "" {
			t.question.SetText(q)
		}
	})
	t.quickQuestion.PlaceHolder = "快速提问"
	t.progress = widget.NewProgressBarInfinite()
	t.progress.Hide()
	footer := container.New(toolbarLayout{}, t.question, container.New(flowLayout{}, t.quickQuestion, t.send, t.cancelBtn), t.progress, t.state)
	return container.New(panelLayout{header: header}, header, t.tabs, footer)
}

func (t *aiTool) loadKey() {
	t.keyLoadErr = ""
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" && t.keyPath != "" {
		var err error
		key, err = ai.LoadKey(t.keyPath)
		if err != nil {
			t.keyLoadErr = err.Error()
		}
	}
	t.key.SetText(key)
}

func (t *aiTool) saveSettings() {
	model := strings.TrimSpace(t.model.Text)
	if model == "" || len(model) > 128 {
		dialog.ShowError(errors.New("请填写有效的模型名称"), t.win)
		return
	}
	key := strings.TrimSpace(t.key.Text)
	if t.keyPath == "" && t.remember.Checked {
		dialog.ShowError(errors.New("无法确定用户凭据目录"), t.win)
		return
	}
	if t.remember.Checked {
		if err := ai.SaveKey(t.keyPath, key); err != nil {
			dialog.ShowError(err, t.win)
			return
		}
	} else if t.keyPath != "" {
		if err := ai.DeleteKey(t.keyPath); err != nil {
			dialog.ShowError(err, t.win)
			return
		}
	}
	t.ws.app.Preferences().SetString("ai.deepseek.model", model)
	t.keyLoadErr = ""
	t.model.SetText(model)
	if t.remember.Checked && key != "" {
		t.state.SetText("设置已保存；API Key 已由 Windows 当前用户加密")
	} else {
		t.state.SetText("设置已保存；API Key 仅用于本窗口当前会话")
	}
}
func (t *aiTool) capture() {
	if t.busy || t.closed {
		return
	}
	sourceErr := ""
	if t.origin != nil && t.origin.closed {
		sourceErr = "来源窗口已关闭，请从需要分析的对象重新打开 AI 助手"
	} else if t.origin != nil && t.origin.selectionOnly && t.target.packet == nil && t.target.event == nil {
		sourceErr = "历史窗口没有选中报文或日志，请先选择要分析的对象"
	}
	if w := t.target.read; w != nil {
		if !slices.Contains(t.ws.windows, w) {
			sourceErr = "来源读取窗口已关闭，请使用当前选择重新选择对象"
		} else if offset := t.target.registerOffset; offset != nil && (int(*offset) < int(w.def.Start) || int(*offset) >= int(w.def.Start)+w.def.Qty) {
			sourceErr = "读取范围已改变，原选中寄存器不在范围内，请使用当前选择重新选择对象"
		}
	}
	t.invalidSource = sourceErr != ""
	t.send.SetText("开始分析")
	t.setOutput("证据已更新，请查看发送内容后开始分析。当前没有这份证据的分析报告。")
	if t.invalidSource {
		t.send.Disable()
		t.preview.SetText("")
		t.snapshot = ai.Snapshot{}
		t.updateEvidenceBrowser()
		t.captureInfo.SetText("")
		t.source.SetText("来源需要重新选择")
		t.state.SetText(sourceErr)
		return
	}
	t.snapshot = t.ws.aiSnapshotFor(t.target, t.includeRaw.Checked, t.includeValues.Checked)
	t.source.SetText(t.snapshot.Source)
	if t.target.read == nil {
		t.includeValues.Disable()
	} else {
		t.includeValues.Enable()
	}
	b, err := t.snapshot.JSON()
	if err != nil {
		t.preview.SetText(err.Error())
		t.snapshot = ai.Snapshot{}
		t.updateEvidenceBrowser()
		t.captureInfo.SetText("证据无法发送：超过限制或内容无效")
		t.send.Disable()
		t.state.SetText(err.Error())
		return
	}
	t.preview.SetText(string(b))
	t.updateEvidenceBrowser()
	t.send.Enable()
	t.captureInfo.SetText(fmt.Sprintf("%s · %d 条证据 · %d 字节", t.snapshot.CapturedAt, len(t.snapshot.Evidence), len(b)))
	if t.keyLoadErr != "" {
		t.state.SetText(t.keyLoadErr)
		t.tabs.Select(t.settingsTab)
	} else {
		t.state.SetText("证据已冻结，请检查发送内容；后续轮询不会改变这份证据")
	}
}
func (t *aiTool) setBusy(busy bool) {
	t.busy = busy
	if busy {
		t.progress.Start()
		t.progress.Show()
		t.refreshEvidence.Disable()
		t.quickQuestion.Disable()
		t.send.Disable()
		t.testBtn.Disable()
		t.refresh.Disable()
		t.includeRaw.Disable()
		t.includeValues.Disable()
		t.key.Disable()
		t.model.Disable()
		t.question.Disable()
		t.save.Disable()
		t.forget.Disable()
		t.remember.Disable()
		t.cancelBtn.Enable()
	} else {
		t.progress.Stop()
		t.progress.Hide()
		t.refreshEvidence.Enable()
		t.quickQuestion.Enable()
		t.send.Enable()
		t.testBtn.Enable()
		t.refresh.Enable()
		t.includeRaw.Enable()
		if t.target.read != nil {
			t.includeValues.Enable()
		} else {
			t.includeValues.Disable()
		}
		t.key.Enable()
		t.model.Enable()
		t.question.Enable()
		t.save.Enable()
		t.forget.Enable()
		if ai.ProtectedStorage {
			t.remember.Enable()
		}
		t.cancelBtn.Disable()
		if _, err := t.snapshot.JSON(); err != nil {
			t.send.Disable()
		}
		if t.invalidSource {
			t.send.Disable()
		}
	}
}
func (t *aiTool) cancelRequest() {
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.busy {
		t.setBusy(false)
		if !t.testOnly && t.exportText == "" {
			t.setOutput("分析已取消；可重新开始分析。")
		}
		message := "已取消；可重新开始分析"
		if t.exportText != "" {
			message += "；已保留上次报告"
		}
		t.state.SetText(message)
	}
}
func (t *aiTool) start(testOnly bool) {
	if t.closed || t.busy || t.ws.closed {
		return
	}
	if !testOnly && t.invalidSource {
		t.state.SetText("来源需要重新选择，请使用当前选择刷新证据")
		return
	}
	key := strings.TrimSpace(t.key.Text)
	if key == "" {
		t.tabs.Select(t.settingsTab)
		t.state.SetText("请先填写 DeepSeek API Key")
		return
	}
	client := &ai.Client{Endpoint: t.endpoint, Key: key, Model: strings.TrimSpace(t.model.Text)}
	snapshot := t.snapshot
	question := strings.TrimSpace(t.question.Text)
	if question == "" {
		question = "请诊断当前证据"
	}
	if testOnly {
		snapshot = ai.Snapshot{ID: "connection-test", Evidence: []ai.Evidence{{ID: "E1", Kind: "synthetic", Data: []byte(`{"connection_test":true,"device_data":false}`)}}}
		question = "这是接口连接测试，只需确认收到合成证据。不要推测设备问题。"
	}
	if _, err := snapshot.JSON(); err != nil {
		t.state.SetText(err.Error())
		return
	}
	t.generation++
	t.testOnly = testOnly
	generation := t.generation
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.cancel = cancel
	t.setBusy(true)
	t.state.SetText("正在请求 DeepSeek，可随时取消（最长 60 秒）")
	if !testOnly {
		t.tabs.SelectIndex(0)
		if t.exportText == "" {
			t.setOutput("正在分析冻结证据…")
		} else {
			t.state.SetText("正在重新分析（最长 60 秒）；上次报告仍可复制和导出")
		}
	}
	go func() {
		defer cancel()
		response, err := client.Analyze(ctx, snapshot, question)
		uiDo(func() {
			if t.closed || t.ws.closed || generation != t.generation {
				return
			}
			t.cancel = nil
			t.setBusy(false)
			if err != nil {
				message := err.Error()
				if errors.Is(err, context.Canceled) {
					message = "请求已取消"
				}
				if errors.Is(err, context.DeadlineExceeded) {
					message = "请求超过 60 秒，请稍后手动重试"
				}
				if !testOnly && t.exportText == "" {
					t.setOutput("分析失败：" + message)
				} else if t.exportText != "" {
					message += "；已保留上次报告"
				}
				t.state.SetText(message)
				return
			}
			usage := response.Metadata()
			if testOnly {
				t.state.SetText("连接成功 · " + usage)
				return
			}
			report := fmt.Sprintf("# AI 诊断报告\n\n对象：%s\n快照：%s\n时间：%s\n服务：https://api.deepseek.com\n%s\n\n问题：%s\n\n%s\n模型判断需要现场核实；检查步骤不会自动执行。\n", snapshot.Source, snapshot.ID, snapshot.CapturedAt, usage, question, response.Result.Markdown())
			t.setOutput(report)
			evidence, _ := snapshot.JSON()
			t.exportText = report + "\n## 发送时冻结证据\n\n```json\n" + string(evidence) + "\n```\n"
			t.copyBtn.Enable()
			t.exportBtn.Enable()
			t.send.SetText("重新分析")
			t.state.SetText("分析完成 · " + usage)
		})
	}()
}
