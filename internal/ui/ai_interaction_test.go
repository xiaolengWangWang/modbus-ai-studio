package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/modbus"
)

func TestAIRefreshCurrentEvidenceKeepsBoundSource(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		first, other := ws.addWindow(defaultDef()), ws.addWindow(defaultDef())
		ws.openAITarget(aiTarget{read: first})
		tool := ws.ai
		tool.includeValues.SetChecked(true)
		ws.setCurrent(other)
		first.mu.Lock()
		first.regs = []uint16{123}
		first.mu.Unlock()
		buttons := findButtons(tool.win.Content(), "更新当前证据")
		if len(buttons) != 1 {
			t.Fatal("refresh for the currently bound source is missing")
		}
		test.Tap(buttons[0])
		if tool.target.read != first || !strings.Contains(tool.preview.Text, "123") {
			t.Error("refresh followed another current window instead of the bound source")
		}
		test.Tap(tool.refresh)
		if tool.target.read != other {
			t.Error("explicit use-current-selection did not change source")
		}
	})
}

func TestAICitationOpensOnlyFrozenLocalEvidence(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		p := modbus.Packet{Time: time.Now(), Mode: modbus.ModeTCP, Address: 42, Count: 2, Status: modbus.StatusTimeout}
		ws.openAI(&p)
		tool := ws.ai
		before := tool.preview.Text
		tool.setOutput("## 证据事实\n\n- 超时 [E1]\n\n![image](https://invalid.example/) [link](https://invalid.example/)")
		p.Address = 99
		var link *widget.HyperlinkSegment
		for _, segment := range tool.reportView.Segments {
			if h, ok := segment.(*widget.HyperlinkSegment); ok {
				if h.Text != "E1" || h.URL != nil || h.OnTapped == nil {
					t.Fatal("untrusted content created an active URL")
				}
				link = h
			}
		}
		if link == nil {
			t.Fatal("valid evidence citation is not clickable")
		}
		link.OnTapped()
		if tool.tabs.Selected() != tool.previewTab || tool.preview.Text != before || !strings.Contains(before, `"address": 42`) {
			t.Error("citation did not open the frozen evidence")
		}
	})
}

func TestProbeResultAIEntryFreezesDetectionStates(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		d := defaultDef()
		d.Start, d.Qty = 10, 4
		res := []int8{1, -1, -3, 0}
		ws.showProbeResult(nil, d, res, context.Canceled)
		buttons := findButtons(ws.win.Canvas().Overlays().Top(), "AI分析结果")
		if len(buttons) != 1 {
			t.Fatal("probe result lacks an AI analysis entry")
		}
		test.Tap(buttons[0])
		if ws.ai == nil {
			t.Fatal("probe AI entry did not open the assistant")
		}
		before, err := ws.ai.snapshot.JSON()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"register_probe", "illegal_address", "no_response", "untested", "cancelled"} {
			if !strings.Contains(string(before), want) {
				t.Errorf("probe snapshot missing %s", want)
			}
		}
		if strings.Contains(string(before), "current_read_definition") {
			t.Error("probe result borrowed live reading state")
		}
		res[0] = -1
		d.Start = 20
		ws.ai.capture()
		after, _ := ws.ai.snapshot.JSON()
		var old, next map[string]any
		json.Unmarshal(before, &old)
		json.Unmarshal(after, &next)
		b1, _ := json.Marshal(old["evidence"])
		b2, _ := json.Marshal(next["evidence"])
		if string(b1) != string(b2) {
			t.Error("completed detection result changed after opening AI")
		}
	})
}

func TestAISettingsActionsWrapAtCompactWidth(t *testing.T) {
	ws := openWS(t, test.NewTempApp(t), false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		tool.tabs.Select(tool.settingsTab)
		tool.win.Resize(fyne.NewSize(520, 600))
		tool.settingsTab.Content.Resize(fyne.NewSize(300, 400))
		base := fyne.CurrentApp().Driver().AbsolutePositionForObject(tool.settingsTab.Content)
		for _, name := range []string{"保存设置", "删除已保存密钥", "测试 DeepSeek 连接"} {
			buttons := findButtons(tool.win.Content(), name)
			if len(buttons) != 1 {
				t.Fatalf("missing setting action %s", name)
			}
			p := fyne.CurrentApp().Driver().AbsolutePositionForObject(buttons[0])
			if p.X+buttons[0].Size().Width > base.X+300+1 {
				t.Errorf("setting action %s is clipped", name)
			}
		}
		outer := tool.settingsTab.Content.(*fyne.Container)
		scroll := outer.Objects[0].(*container.Scroll)
		outer.Resize(fyne.NewSize(300, 180))
		layout := outer.Layout.(wrappingScrollLayout)
		note := layout.content.Objects[len(layout.content.Objects)-1]
		if note.Position().Y+note.Size().Height > scroll.Content.MinSize().Height+1 {
			t.Error("wrapped settings exceed the scroll extent")
		}
		scroll.ScrollToBottom()
		if scroll.Offset.Y <= 0 {
			t.Error("compact settings cannot scroll to bottom")
		}
	})
}
