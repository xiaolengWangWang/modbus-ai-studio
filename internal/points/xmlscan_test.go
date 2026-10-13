package points

import (
	"context"
	"reflect"
	"testing"
)

// 扫描器要认得 Excel 和各种导出工具写出的写法：实体、CDATA、带格式的文字、注音、命名空间前缀、
// 单引号属性、属性值里的 >、注释和 XML 声明、\r\n 换行。
func TestXMLScanSheetAndSharedStrings(t *testing.T) {
	sst := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\r\n<!-- 导出工具写的注释 -->" +
		`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="5">` +
		`<si><t>属性标识</t></si>` +
		`<si><r><rPr><b/></rPr><t>二次</t></r><r><t xml:space="preserve">供温 </t></r><rPh sb="0" eb="1"><t>注音</t></rPh></si>` +
		`<si><t>A&amp;B &lt;1&gt; &quot;q&quot; &apos;s&apos; &#21517;&#x79F0;</t></si>` +
		`<si><t><![CDATA[x<y&z]]></t></si>` +
		"<si><t>第一行\r\n第二行</t></si>" +
		`<si/>` +
		`</sst>`)
	shared, err := parseSharedStrings(context.Background(), sst)
	want := []string{"属性标识", "二次供温 ", `A&B <1> "q" 's' 名称`, "x<y&z", "第一行\n第二行", ""}
	if err != nil || !reflect.DeepEqual(shared, want) {
		t.Fatalf("共享字符串：%q，%v", shared, err)
	}

	sheet := []byte(`<?xml version="1.0"?><x:worksheet xmlns:x="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><x:sheetData>` +
		`<x:row r="1"><x:c r="A1" t="s"><x:v>0</x:v></x:c><x:c r='B1' t='s' s="3"><x:v>1</x:v></x:c></x:row>` +
		// 属性值里的 >、内联字符串、公式只取缓存的值、40021.0 写回成整数、自闭合的空单元格
		`<x:row r="2"><x:c r="A2" t="inlineStr" note="a>b"><x:is><x:t>4x0021:REAL</x:t></x:is></x:c>` +
		`<x:c r="B2"><x:f>SUM(1,2)</x:f><x:v>40021.0</x:v></x:c><x:c r="C2" s="1"/><x:c r="D2" t="s"><x:v>2</x:v></x:c></x:row>` +
		`<x:row r="3"><x:c r="E3" t="str"><x:v>文字</x:v></x:c></x:row>` +
		`</x:sheetData></x:worksheet>`)
	rows, err := parseSheet(context.Background(), sheet, shared, false, nil)
	wantRows := [][]string{{"属性标识", "二次供温 "}, {"4x0021:REAL", "40021", "", `A&B <1> "q" 's' 名称`}, {"", "", "", "", "文字"}}
	if err != nil || !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("工作表：%q，%v", rows, err)
	}
	if head, err := parseSheet(context.Background(), sheet, shared, true, nil); err != nil || len(head) != 1 {
		t.Errorf("只看表头应只读第一行：%q，%v", head, err)
	}
}

// 格式坏了要报错，不能悄悄少读几行。
func TestXMLScanRejectsBrokenXML(t *testing.T) {
	for name, b := range map[string]string{
		"标签没写完":   `<worksheet><sheetData><row><c r="A1"`,
		"引号没关":    `<worksheet><sheetData><row><c r="A1><v>1</v></c></row></sheetData></worksheet>`,
		"不认识的实体":  `<worksheet><sheetData><row><c r="A1" t="str"><v>&nbsp;</v></c></row></sheetData></worksheet>`,
		"注释没关":    `<worksheet><!-- <sheetData>`,
		"CDATA没关": `<worksheet><sheetData><row><c r="A1" t="str"><v><![CDATA[1</v></c>`,
	} {
		if _, err := parseSheet(context.Background(), []byte(b), nil, false, nil); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}
