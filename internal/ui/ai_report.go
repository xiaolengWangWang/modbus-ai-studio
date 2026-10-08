package ui

import (
	"io"
	"regexp"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"modbus-ai-studio/internal/ai"
)

// Keep every model-provided value as plain text. Markdown image/link renderers
// would introduce unintended network calls or active content in a report.
func aiReportSegments(text string) []widget.RichTextSegment {
	blocks := strings.Split(text, "\n\n")
	out := make([]widget.RichTextSegment, 0, len(blocks))
	for _, block := range blocks {
		style := widget.RichTextStyleParagraph
		if block == "# AI 诊断报告" {
			block = "AI 诊断报告"
			style = widget.RichTextStyleHeading
			style.Inline = false
		}
		for _, heading := range []string{"诊断结论", "证据事实", "可能原因（待验证）", "下一步人工检查"} {
			if block == "## "+heading {
				block = heading
				style = widget.RichTextStyleParagraph
				style.TextStyle.Bold = true
			}
		}
		out = append(out, &widget.TextSegment{Text: block, Style: style})
	}
	return out
}

func (t *aiTool) setOutput(text string) {
	t.reportText = text
	t.exportText = ""
	t.reportView.Segments = aiReportEvidenceSegments(text, t.snapshot, t.showEvidence)
	t.reportView.Refresh()
	if t.copyBtn != nil {
		t.copyBtn.Disable()
	}
	if t.exportBtn != nil {
		t.exportBtn.Disable()
	}
}

var aiCitationPattern = regexp.MustCompile(`\[(E[0-9]+(?:,\s*E[0-9]+)*)\]`)

func aiReportEvidenceSegments(text string, snapshot ai.Snapshot, open func(string)) []widget.RichTextSegment {
	known := map[string]bool{}
	for _, e := range snapshot.Evidence {
		known[e.ID] = true
	}
	var out []widget.RichTextSegment
	for _, segment := range aiReportSegments(text) {
		s := segment.(*widget.TextSegment)
		start := 0
		style := s.Style
		style.Inline = true
		for _, m := range aiCitationPattern.FindAllStringSubmatchIndex(s.Text, -1) {
			ids := strings.Split(s.Text[m[2]:m[3]], ",")
			valid := true
			for _, id := range ids {
				if !known[strings.TrimSpace(id)] {
					valid = false
				}
			}
			if !valid {
				continue
			}
			out = append(out, &widget.TextSegment{Text: s.Text[start:m[0]] + "[", Style: style})
			for i, value := range ids {
				id := strings.TrimSpace(value)
				if i > 0 {
					out = append(out, &widget.TextSegment{Text: ", ", Style: style})
				}
				out = append(out, &widget.HyperlinkSegment{Text: id, OnTapped: func() { open(id) }})
			}
			out = append(out, &widget.TextSegment{Text: "]", Style: style})
			start = m[1]
		}
		out = append(out, &widget.TextSegment{Text: s.Text[start:], Style: s.Style})
	}
	return out
}

func writeAIReport(w io.WriteCloser, text string) error {
	n, err := w.Write([]byte(text))
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	closeErr := w.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (t *aiTool) exportReport() {
	if t.exportText == "" || t.closed {
		return
	}
	// Freeze the completed report before opening the save dialog. Another
	// selection or request cannot replace the text while a path is chosen.
	report := t.exportText
	d := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
		if err != nil {
			if w != nil {
				w.Close()
			}
			dialog.ShowError(err, t.win)
			return
		}
		if w == nil {
			return
		}
		if err = writeAIReport(w, report); err != nil {
			dialog.ShowError(err, t.win)
			return
		}
		if !t.closed {
			t.state.SetText("诊断报告已导出（包含发送时的冻结证据）")
		}
	}, t.win)
	d.SetFileName("modbus-ai-report-" + time.Now().Format("20060102-150405") + ".md")
	d.Show()
}
