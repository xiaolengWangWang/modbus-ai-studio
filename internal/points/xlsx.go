package points

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// 最小的 xlsx 读取：只取各工作表单元格的文字，用于导入点表。不处理公式（取缓存的结果）、
// 合并单元格和日期格式。工作表和共享字符串表用 xmlScan 直接扫描字节（见 xmlscan.go），工作簿清单这类小文件仍用 encoding/xml。

const maxXLSXBytes = 128 << 20 // 解压后的总 XML 大小，避免小文件解压占满内存。
const maxXLSXCells = 4_000_000

// workbook 是打开的 xlsx：共享字符串已读出，工作表按需读取。
type workbook struct {
	files     map[string]*zip.File
	remaining int64
	shared    []string
	sheets    []workbookSheet
}

type workbookSheet struct{ name, path string }

// openXLSX 读出共享字符串和工作表清单（按工作簿里的顺序），工作表内容等用到时再读。
func openXLSX(ctx context.Context, data []byte) (*workbook, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("不是 xlsx 文件")
	}
	wb := &workbook{files: map[string]*zip.File{}, remaining: maxXLSXBytes}
	for _, f := range z.File {
		wb.files[f.Name] = f
	}
	if _, ok := wb.files["xl/sharedStrings.xml"]; ok {
		b, err := wb.read("xl/sharedStrings.xml")
		if err != nil {
			return nil, err
		}
		if wb.shared, err = parseSharedStrings(ctx, b); err != nil {
			return nil, err
		}
	}
	b, err := wb.read("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	var book struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(b, &book); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	if _, ok := wb.files["xl/_rels/workbook.xml.rels"]; ok {
		b, err := wb.read("xl/_rels/workbook.xml.rels")
		if err != nil {
			return nil, err
		}
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
	for i, s := range book.Sheets {
		p := targets[s.RID]
		if p == "" {
			p = fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		}
		wb.sheets = append(wb.sheets, workbookSheet{s.Name, p})
	}
	return wb, nil
}

// read 解压一个文件，所有文件合计不超过 maxXLSXBytes。
func (wb *workbook) read(name string) ([]byte, error) {
	f, ok := wb.files[name]
	if !ok {
		return nil, fmt.Errorf("xlsx 缺少 %s", name)
	}
	if f.UncompressedSize64 > uint64(wb.remaining) {
		return nil, errors.New("xlsx 解压后超过 128 MB，请删除无关工作表和空白格式区域，或拆分后导入")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, wb.remaining+1))
	if int64(len(data)) > wb.remaining {
		return nil, errors.New("xlsx 解压后超过 128 MB，请拆分后导入")
	}
	wb.remaining -= int64(len(data))
	return data, err
}

// parseSharedStrings 读共享字符串表：每个 <si> 是一段文字，普通文字在 <t> 里，带格式的分成多个 <r><t>；
// <rPh> 是注音，不算正文。
func parseSharedStrings(ctx context.Context, b []byte) ([]string, error) {
	sc := xmlScan{b: b}
	var out []string
	var text strings.Builder
	inSI, inT, phonetic := false, false, 0
	for n := 0; ; n++ {
		if n&0xFFFF == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tok, err := sc.next()
		if err != nil {
			return nil, err
		}
		switch tok.kind {
		case tokEOF:
			return out, nil
		case tokStart:
			switch string(tok.name) {
			case "si":
				inSI = true
				text.Reset()
				if tok.self {
					inSI = false
					out = append(out, "")
				}
			case "rPh":
				if !tok.self {
					phonetic++
				}
			case "t":
				inT = inSI && phonetic == 0 && !tok.self
			}
		case tokText:
			if inT {
				if err := appendText(&text, tok.text, tok.cdata); err != nil {
					return nil, err
				}
			}
		case tokEnd:
			switch string(tok.name) {
			case "si":
				inSI = false
				out = append(out, text.String())
			case "rPh":
				phonetic--
			case "t":
				inT = false
			}
		}
	}
}

// parseSheet 读工作表的单元格文字，按行返回；firstRowOnly 时只读第一个非空行（看表头用）。
// progress 报告读到工作表 XML 的第几个字节，可为空。
func parseSheet(ctx context.Context, b []byte, shared []string, firstRowOnly bool, progress func(done, total int)) ([][]string, error) {
	sc := xmlScan{b: b}
	var (
		rows                [][]string
		row                 []string
		ref, typ            string
		text                strings.Builder
		inData, inRow, inC  bool
		inV, inIs, inT      bool
		phonetic            int
		nextCol, cellsTotal int
	)
	finishCell := func() error {
		col := nextCol
		if ref != "" {
			col = colIndex(ref) // 空单元格不写进文件，按引用补齐
		}
		if col < 0 || col >= 16384 {
			return fmt.Errorf("单元格引用 %q 无效", ref)
		}
		nextCol = col + 1
		v := text.String()
		switch typ {
		case "s":
			i, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || i < 0 || i >= len(shared) {
				return fmt.Errorf("单元格 %s 引用了不存在的字符串", ref)
			}
			v = shared[i]
		case "", "n":
			// 有的导出工具把整数存成 40021.0，地址、长度按整数解析会出错；按 Excel 显示的样子写回
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				v = strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
		// Excel 会为末列或整张表的空白区域写格式，不能因此填充巨大的空数组。
		if strings.TrimSpace(v) == "" {
			return nil
		}
		if col >= len(row) {
			if cellsTotal+col+1 > maxXLSXCells {
				return errors.New("工作表有效单元格区域过大，请删除无关列或拆分后导入")
			}
			row = append(row, make([]string, col+1-len(row))...)
		}
		row[col] = v
		return nil
	}
	end := func(name string) error {
		switch name {
		case "v":
			inV = false
		case "t":
			inT = false
		case "is":
			inIs = false
		case "rPh":
			phonetic--
		case "c":
			if inC {
				inC = false
				return finishCell()
			}
		case "row":
			if inRow {
				inRow = false
				if len(row) > 0 {
					cellsTotal += len(row)
					rows = append(rows, row)
				}
			}
		case "sheetData":
			inData = false
		}
		return nil
	}
	for n := 0; ; n++ {
		if n&0xFFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if progress != nil {
				progress(sc.pos, len(b))
			}
		}
		tok, err := sc.next()
		if err != nil {
			return nil, err
		}
		switch tok.kind {
		case tokEOF:
			return rows, nil
		case tokText:
			if inV || inT {
				if err := appendText(&text, tok.text, tok.cdata); err != nil {
					return nil, err
				}
			}
			continue
		case tokEnd:
			if err := end(string(tok.name)); err != nil {
				return nil, err
			}
		case tokStart:
			name := string(tok.name)
			switch name {
			case "sheetData":
				inData = true
			case "row":
				if inData {
					inRow, row, nextCol = true, nil, 0
				}
			case "c":
				if inRow {
					inC = true
					text.Reset()
					if ref, err = attr(tok.attrs, "r"); err == nil {
						typ, err = attr(tok.attrs, "t")
					}
					if err != nil {
						return nil, err
					}
				}
			case "v":
				inV = inC && typ != "inlineStr"
			case "is":
				inIs = inC
			case "rPh":
				phonetic++
			case "t":
				inT = inIs && phonetic == 0
			}
			if tok.self {
				if err := end(name); err != nil {
					return nil, err
				}
			}
		}
		if firstRowOnly && len(rows) > 0 {
			return rows, nil
		}
	}
}

// colIndex 把 “AB12” 这样的单元格引用换成从 0 开始的列号。
func colIndex(ref string) int {
	n := 0
	for _, ch := range ref {
		if ch < 'A' || ch > 'Z' {
			break
		}
		n = n*26 + int(ch-'A'+1)
		if n > 16384 {
			return -1
		}
	}
	return n - 1
}
