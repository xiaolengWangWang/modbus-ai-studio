package points

import (
	"context"
	"fmt"
	"testing"
)

func TestParseSheetSkipsBlankFormattingCells(t *testing.T) {
	b := []byte(`<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>地址</t></is></c><c r="B1" t="inlineStr"><is><t>名称</t></is></c><c r="C1" t="inlineStr"><is><t>类型</t></is></c><c r="XFD1" s="1"/></row><row r="2"><c r="A2"><v>40001</v></c><c r="B2" t="inlineStr"><is><t>温度</t></is></c><c r="C2" t="inlineStr"><is><t>UINT16</t></is></c><c r="XFD2" s="1"/></row><row r="1048576"><c r="XFD1048576" s="1"/></row></sheetData></worksheet>`)
	rows, err := parseSheet(context.Background(), b, nil, false, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("只有格式的空行不应进入点表：%d, %v", len(rows), err)
	}
	for _, row := range rows {
		if len(row) != 3 {
			t.Errorf("末列空白格式不应使每行扩张到 16384 列：%d", len(row))
		}
	}
	imp, err := ParseRows(rows)
	if err != nil || len(imp.Points) != 1 || imp.Points[0].Name != "温度" {
		t.Fatalf("有效点位应仍可导入：%+v, %v", imp, err)
	}
}

func TestParseSheetKeepsInternalBlankColumnsWithoutReferences(t *testing.T) {
	rows, err := parseSheet(context.Background(), []byte(`<worksheet><sheetData><row><c><v>40001</v></c><c/><c t="inlineStr"><is><t>UINT16</t></is></c></row></sheetData></worksheet>`), nil, false, nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 3 || rows[0][1] != "" || rows[0][2] != "UINT16" {
		t.Fatalf("删除尾部空白不能改变内部空列的位置：%v, %v", rows, err)
	}
}

func TestParseSheetRejectsInvalidReferences(t *testing.T) {
	for _, ref := range []string{"123", "XFE1", "ZZZZZZZZZZZZ1"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := parseSheet(context.Background(), []byte(fmt.Sprintf(`<worksheet><sheetData><row><c r="%s"><v>1</v></c></row></sheetData></worksheet>`, ref)), nil, false, nil); err == nil {
				t.Fatal("无效单元格引用应返回错误，避免越界或大内存分配")
			}
		})
	}
}
