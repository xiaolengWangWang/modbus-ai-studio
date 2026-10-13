package points

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// xmlScan 是只够读 xlsx 的 XML 扫描器。工作表和共享字符串表都是程序写出的规整 XML，用 encoding/xml
// 逐个记号读要为每个标签分配好几个小对象，20 万行的表要十秒；这里直接在字节上找标签，快好几倍。
// 处理元素、属性、文字、实体和 CDATA，跳过注释、处理指令和 DOCTYPE；不校验标签是否配对。
type xmlScan struct {
	b   []byte
	pos int
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokStart
	tokEnd
	tokText
)

type xmlTok struct {
	kind  tokKind
	name  []byte // 本地名，去掉了命名空间前缀
	attrs []byte // 开始标签里名字之后的部分，用 attr 取值
	self  bool   // <c/> 这样的自闭合标签，相当于开始后紧接着结束
	text  []byte // 文字，实体还没解码（CDATA 除外），用 appendText 解码
	cdata bool
}

var errXML = errors.New("xlsx 里的 XML 不完整或格式不对")

func (s *xmlScan) next() (xmlTok, error) {
	for {
		if s.pos >= len(s.b) {
			return xmlTok{kind: tokEOF}, nil
		}
		rest := s.b[s.pos:]
		if rest[0] != '<' {
			i := bytes.IndexByte(rest, '<')
			if i < 0 {
				i = len(rest)
			}
			s.pos += i
			return xmlTok{kind: tokText, text: rest[:i]}, nil
		}
		skip := func(open, close string) (bool, error) {
			if !bytes.HasPrefix(rest, []byte(open)) {
				return false, nil
			}
			i := bytes.Index(rest[len(open):], []byte(close))
			if i < 0 {
				return true, errXML
			}
			s.pos += len(open) + i + len(close)
			return true, nil
		}
		if bytes.HasPrefix(rest, []byte("<![CDATA[")) {
			i := bytes.Index(rest, []byte("]]>"))
			if i < 0 {
				return xmlTok{}, errXML
			}
			s.pos += i + 3
			return xmlTok{kind: tokText, text: rest[9:i], cdata: true}, nil
		}
		if ok, err := skip("<!--", "-->"); ok || err != nil {
			if err != nil {
				return xmlTok{}, err
			}
			continue
		}
		if ok, err := skip("<?", "?>"); ok || err != nil {
			if err != nil {
				return xmlTok{}, err
			}
			continue
		}
		if ok, err := skip("<!", ">"); ok || err != nil {
			if err != nil {
				return xmlTok{}, err
			}
			continue
		}
		// 找标签结尾的 '>'：属性值里可以有 '>'，引号里的不算
		end, quote := -1, byte(0)
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '"' || c == '\'':
				quote = c
			case c == '>':
				end = i
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return xmlTok{}, errXML
		}
		tag := rest[1:end]
		s.pos += end + 1
		t := xmlTok{kind: tokStart}
		if len(tag) > 0 && tag[0] == '/' {
			t.kind, tag = tokEnd, tag[1:]
		} else if len(tag) > 0 && tag[len(tag)-1] == '/' {
			t.self, tag = true, tag[:len(tag)-1]
		}
		n := 0
		for n < len(tag) && !isXMLSpace(tag[n]) {
			n++
		}
		if n == 0 {
			return xmlTok{}, errXML
		}
		t.name, t.attrs = tag[:n], tag[n:]
		if i := bytes.LastIndexByte(t.name, ':'); i >= 0 {
			t.name = t.name[i+1:]
		}
		return t, nil
	}
}

func isXMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// attr 取开始标签里某个属性（按本地名）的值，没有时返回空。
func attr(attrs []byte, name string) (string, error) {
	for i := 0; i < len(attrs); {
		for i < len(attrs) && isXMLSpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) {
			break
		}
		start := i
		for i < len(attrs) && attrs[i] != '=' && !isXMLSpace(attrs[i]) {
			i++
		}
		key := attrs[start:i]
		for i < len(attrs) && isXMLSpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] != '=' {
			return "", errXML
		}
		i++
		for i < len(attrs) && isXMLSpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] != '"' && attrs[i] != '\'' {
			return "", errXML
		}
		q := attrs[i]
		j := bytes.IndexByte(attrs[i+1:], q)
		if j < 0 {
			return "", errXML
		}
		value := attrs[i+1 : i+1+j]
		i += j + 2
		if k := bytes.LastIndexByte(key, ':'); k >= 0 {
			key = key[k+1:]
		}
		if string(key) == name {
			var sb strings.Builder
			if err := appendText(&sb, value, false); err != nil {
				return "", err
			}
			return sb.String(), nil
		}
	}
	return "", nil
}

// appendText 把文字解码后接到 sb 后面：换行统一成 \n，解码 &amp; &#10; 这样的实体（CDATA 原样）。
func appendText(sb *strings.Builder, raw []byte, cdata bool) error {
	for len(raw) > 0 {
		i := bytes.IndexAny(raw, "&\r")
		if cdata {
			i = bytes.IndexByte(raw, '\r')
		}
		if i < 0 {
			sb.Write(raw)
			return nil
		}
		sb.Write(raw[:i])
		raw = raw[i:]
		if raw[0] == '\r' {
			sb.WriteByte('\n')
			raw = raw[1:]
			if len(raw) > 0 && raw[0] == '\n' {
				raw = raw[1:]
			}
			continue
		}
		end := bytes.IndexByte(raw, ';')
		if end < 0 || end > 12 {
			return errXML
		}
		name := string(raw[1:end])
		raw = raw[end+1:]
		switch name {
		case "amp":
			sb.WriteByte('&')
		case "lt":
			sb.WriteByte('<')
		case "gt":
			sb.WriteByte('>')
		case "quot":
			sb.WriteByte('"')
		case "apos":
			sb.WriteByte('\'')
		default:
			var n uint64
			var err error
			switch {
			case strings.HasPrefix(name, "#x"):
				n, err = strconv.ParseUint(name[2:], 16, 32)
			case strings.HasPrefix(name, "#"):
				n, err = strconv.ParseUint(name[1:], 10, 32)
			default:
				err = errXML
			}
			if err != nil || !utf8.ValidRune(rune(n)) {
				return errors.New("xlsx 里有不认识的 XML 实体 &" + name + ";")
			}
			sb.WriteRune(rune(n))
		}
	}
	return nil
}
