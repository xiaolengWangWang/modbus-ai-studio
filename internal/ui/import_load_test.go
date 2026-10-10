package ui

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/test"

	"modbus-ai-studio/internal/modbus"
)

type importLoadReader struct {
	io.ReadCloser
	uri fyne.URI
}

func (r *importLoadReader) URI() fyne.URI { return r.uri }

func TestPointImportThousandRowFiles(t *testing.T) {
	rows := [][]string{{"地址", "名称", "类型"}}
	for i := range 1000 {
		rows = append(rows, []string{fmt.Sprintf("4x%05d", 1+i*60), fmt.Sprintf("点%d", i), "UINT16"})
	}
	var csvData bytes.Buffer
	w := csv.NewWriter(&csvData)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"points.csv", csvData.Bytes()},
		{"points.xlsx", makeXLSX(t, rows)},
	} {
		t.Run(file.name, func(t *testing.T) {
			ws := openWS(t, test.NewTempApp(t), false)
			r := &importLoadReader{ReadCloser: io.NopCloser(bytes.NewReader(file.data)), uri: storage.NewFileURI(file.name)}
			locked(func() { ws.startPointImport(r) })
			waitFor(t, 10*time.Second, "1,000-row point import", func() bool { return ws.importTask == nil })
			locked(func() {
				if len(ws.points) != 1000 {
					t.Fatalf("expected all 1,000 points to be retained, got %d", len(ws.points))
				}
				p, ok := ws.points.get(modbus.AreaHoldingRegisters, 59940)
				if !ok || p.Name != "点999" || p.Type != modbus.TypeUint16 {
					t.Errorf("last point was not retained correctly: %+v, found=%v", p, ok)
				}
				if len(ws.windows) > 16 {
					t.Errorf("expected at most 16 automatic windows, got %d", len(ws.windows))
				}
				if report := overlayText(ws); !strings.Contains(report, "未自动建窗") {
					t.Errorf("import report must explain the window limit: %s", report)
				}
			})
		})
	}
}
