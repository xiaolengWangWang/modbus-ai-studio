package points

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"reflect"
	"strings"
	"testing"
	"time"
)

// attrRows 生成 n 行平台设备属性表：地址每个 REAL 隔 2，4x 用完后接着用 3x。
func attrRows(n int) [][]string {
	rows := [][]string{{"属性标识", "属性名称", "读写模式", "单位", "计算公式", "数据类型", "模式"}}
	for i := range n {
		id := fmt.Sprintf("4x%05d:REAL", 1+i*2)
		if i*2 >= 60000 {
			id = fmt.Sprintf("3x%05d:REAL", 1+i*2-60000)
		}
		rows = append(rows, []string{id, fmt.Sprintf("点%d", i), "1", "℃", "x*0.1", "2", "0"})
	}
	return rows
}

func csvOf(rows [][]string) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		b.WriteString(strings.Join(r, ",") + "\n")
	}
	return b.Bytes()
}

// xlsxOf 生成只有一个工作表的 xlsx，文字都放进共享字符串表，和 Excel 存出来的一样。
func xlsxOf(t testing.TB, rows [][]string) []byte {
	t.Helper()
	var sheet, sst strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	sst.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	n := 0
	for r, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, r+1)
		for c, v := range row {
			fmt.Fprintf(&sheet, `<c r="%c%d" t="s"><v>%d</v></c>`, 'A'+c, r+1, n)
			fmt.Fprintf(&sst, `<si><t>%s</t></si>`, html.EscapeString(v))
			n++
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	sst.WriteString(`</sst>`)
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="属性" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet.String(),
		"xl/sharedStrings.xml":       sst.String(),
	} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 同一张表存成 CSV 和 xlsx，解析结果一样；进度只增不减，最后到 1。
func TestParseProgressAndFormatsAgree(t *testing.T) {
	rows := attrRows(5000)
	var want Import
	for _, f := range []struct {
		name string
		data []byte
	}{{"a.csv", csvOf(rows)}, {"a.xlsx", xlsxOf(t, rows)}} {
		var got []float64
		imp, err := Parse(context.Background(), f.name, f.data, func(p float64) { got = append(got, p) })
		if err != nil || len(imp.Points) != 5000 {
			t.Fatalf("%s：%d 点，%v", f.name, len(imp.Points), err)
		}
		for i := 1; i < len(got); i++ {
			if got[i] < got[i-1] {
				t.Fatalf("%s：进度倒退 %v", f.name, got)
			}
		}
		if len(got) < 3 || got[len(got)-1] < 0.99 || got[len(got)-1] > 1 {
			t.Errorf("%s：进度应分几次报告、最后到 1：%v", f.name, got)
		}
		if want.Points == nil {
			want = imp
		} else if !reflect.DeepEqual(imp, want) {
			t.Errorf("xlsx 和 CSV 解析结果不同")
		}
	}
}

// 解析到一半取消，很快返回 context.Canceled，不把整个文件读完。
func TestParseCancel(t *testing.T) {
	rows := attrRows(100000)
	for _, f := range []struct {
		name string
		data []byte
	}{{"a.csv", csvOf(rows)}, {"a.xlsx", xlsxOf(t, rows)}} {
		ctx, cancel := context.WithCancel(context.Background())
		var at float64
		start := time.Now()
		_, err := Parse(ctx, f.name, f.data, func(p float64) {
			if p > 0.2 && at == 0 {
				at = p
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s：取消后应返回 context.Canceled：%v", f.name, err)
		}
		if at == 0 || at > 0.9 {
			t.Errorf("%s：应在解析中途取消，取消时进度 %v", f.name, at)
		}
		t.Logf("%s：进度 %.0f%% 时取消，用时 %v", f.name, at*100, time.Since(start))
	}
}

// 大表解析速度（go test -bench Parse ./internal/points）：20 万行属性表的 CSV 和 xlsx。
func BenchmarkParse(b *testing.B) {
	rows := attrRows(200000)
	for _, f := range []struct {
		name string
		data []byte
	}{{"a.csv", csvOf(rows)}, {"a.xlsx", xlsxOf(b, rows)}} {
		b.Run(f.name, func(b *testing.B) {
			b.SetBytes(int64(len(f.data)))
			for b.Loop() {
				if _, err := ParseFile(f.name, f.data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
