package ui

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// 最小的 xlsx 读取：只取各工作表单元格的文字，用于导入点表。不处理公式（取缓存的结果）、
// 合并单元格和日期格式。

type xlsxSheet struct {
	name string
	rows [][]string
}

// readXLSX 按工作簿里的顺序返回全部工作表。
func readXLSX(data []byte) ([]xlsxSheet, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("不是 xlsx 文件")
	}
	files := map[string]*zip.File{}
	for _, f := range z.File {
		files[f.Name] = f
	}
	read := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("xlsx 缺少 %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}

	var shared []string
	if b, err := read("xl/sharedStrings.xml"); err == nil {
		var sst struct {
			Items []xlsxText `xml:"si"`
		}
		if err := xml.Unmarshal(b, &sst); err != nil {
			return nil, err
		}
		for _, it := range sst.Items {
			shared = append(shared, it.text())
		}
	}

	b, err := read("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(b, &wb); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	if b, err := read("xl/_rels/workbook.xml.rels"); err == nil {
		var rels struct {
			Rels []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		if err := xml.Unmarshal(b, &rels); err != nil {
			return nil, err
		}
		for _, r := range rels.Rels {
			t := strings.TrimPrefix(r.Target, "/")
			if !strings.HasPrefix(t, "xl/") {
				t = path.Join("xl", t)
			}
			targets[r.ID] = t
		}
	}

	var out []xlsxSheet
	for i, s := range wb.Sheets {
		name := targets[s.RID]
		if name == "" {
			name = fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		}
		b, err := read(name)
		if err != nil {
			return nil, err
		}
		rows, err := parseSheet(b, shared)
		if err != nil {
			return nil, fmt.Errorf("工作表“%s”：%w", s.Name, err)
		}
		out = append(out, xlsxSheet{name: s.Name, rows: rows})
	}
	return out, nil
}

// xlsxText 是共享字符串或行内字符串：普通文字在 <t> 里，带格式的文字分成多个 <r><t>。
type xlsxText struct {
	T    string `xml:"t"`
	Runs []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

func (x xlsxText) text() string {
	if len(x.Runs) == 0 {
		return x.T
	}
	var sb strings.Builder
	for _, r := range x.Runs {
		sb.WriteString(r.T)
	}
	return sb.String()
}

func parseSheet(b []byte, shared []string) ([][]string, error) {
	var ws struct {
		Rows []struct {
			Cells []struct {
				Ref    string   `xml:"r,attr"`
				Type   string   `xml:"t,attr"`
				Value  string   `xml:"v"`
				Inline xlsxText `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(b, &ws); err != nil {
		return nil, err
	}
	var rows [][]string
	for _, r := range ws.Rows {
		var row []string
		for _, c := range r.Cells {
			col := len(row)
			if c.Ref != "" {
				col = colIndex(c.Ref) // 空单元格不写进文件，按引用补齐
			}
			for len(row) <= col {
				row = append(row, "")
			}
			v := c.Value
			switch c.Type {
			case "s":
				var i int
				if _, err := fmt.Sscan(v, &i); err != nil || i < 0 || i >= len(shared) {
					return nil, fmt.Errorf("单元格 %s 引用了不存在的字符串", c.Ref)
				}
				v = shared[i]
			case "inlineStr":
				v = c.Inline.text()
			case "", "n":
				// 有的导出工具把整数存成 40021.0，地址、长度按整数解析会出错；按 Excel 显示的样子写回
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					v = strconv.FormatFloat(f, 'f', -1, 64)
				}
			}
			row[col] = v
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// colIndex 把 “AB12” 这样的单元格引用换成从 0 开始的列号。
func colIndex(ref string) int {
	n := 0
	for _, ch := range ref {
		if ch < 'A' || ch > 'Z' {
			break
		}
		n = n*26 + int(ch-'A'+1)
	}
	return n - 1
}
