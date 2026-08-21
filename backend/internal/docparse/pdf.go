package docparse

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

var (
	rePDFPage     = regexp.MustCompile(`/Type\s*/Page[^s]`)
	rePDFLength   = regexp.MustCompile(`/Length\s+(\d+)`)
	rePDFFilter   = regexp.MustCompile(`/Filter\s*(\[[^\]]*\]|/[[:alnum:]]+)`)
	rePDFEncrypt  = regexp.MustCompile(`(?m)/Encrypt\s+\d+\s+\d+\s+R`)
	rePDFFilterNm = regexp.MustCompile(`/[[:alnum:]]+`)
)

func extractPDF(data []byte) (Result, error) {
	pages := len(rePDFPage.FindAllIndex(data, -1))
	if rePDFEncrypt.Match(data) {
		return Result{Kind: PDF, Pages: pages}, ErrEncrypted
	}
	var chunks []string
	for _, s := range pdfStreams(data) {
		if s.skip {
			continue
		}
		decoded, err := pdfDecode(s)
		if err != nil || len(decoded) == 0 {
			continue
		}
		if t := pdfContentText(decoded); t != "" {
			chunks = append(chunks, t)
		}
	}
	if len(chunks) == 0 {
		return Result{Kind: PDF, Pages: pages}, ErrNoText
	}
	var b strings.Builder
	if len(chunks) == 1 {
		b.WriteString(chunks[0])
	} else {
		for i, c := range chunks {
			fmt.Fprintf(&b, "--- page %d ---\n%s\n", i+1, c)
		}
	}
	return Result{Kind: PDF, Pages: pages, Text: strings.TrimSpace(b.String())}, nil
}

type pdfStream struct {
	dict, data []byte
	skip       bool
}

func pdfStreams(data []byte) []pdfStream {
	var out []pdfStream
	for i := 0; i < len(data); {
		j := indexWord(data, i, []byte("stream"))
		if j < 0 {
			break
		}
		after := j + len("stream")
		if after < len(data) && data[after] == '\r' {
			after++
		}
		if after < len(data) && data[after] == '\n' {
			after++
		}
		dictStart := j - 2500
		if dictStart < 0 {
			dictStart = 0
		}
		dict := data[dictStart:j]
		s := pdfStream{dict: dict}
		if pdfSkipStream(dict) {
			s.skip = true
		}
		length := pdfStreamLength(dict)
		var payload []byte
		if length >= 0 && after+length <= len(data) {
			payload = data[after : after+length]
		} else {
			k := bytes.Index(data[after:], []byte("endstream"))
			if k < 0 {
				break
			}
			payload = bytes.TrimRight(data[after:after+k], "\r\n")
			length = len(payload)
		}
		s.data = payload
		out = append(out, s)
		i = after + length
		if i < len(data) {
			if k := bytes.Index(data[i:], []byte("endstream")); k >= 0 && k < 8 {
				i += k + len("endstream")
			}
		}
	}
	return out
}

func pdfStreamLength(dict []byte) int {
	m := rePDFLength.FindSubmatchIndex(dict)
	if m == nil {
		return -1
	}
	rest := bytes.TrimLeft(dict[m[1]:], " \t")
	if len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
		return -1 // /Length 10 0 R — indirect
	}
	n, err := strconv.Atoi(string(dict[m[2]:m[3]]))
	if err != nil {
		return -1
	}
	return n
}

func pdfSkipStream(dict []byte) bool {
	if bytes.Contains(dict, []byte("/Subtype /Image")) || bytes.Contains(dict, []byte("/Subtype/Image")) {
		return true
	}
	if bytes.Contains(dict, []byte("/Type /XRef")) || bytes.Contains(dict, []byte("/Type/XRef")) {
		return true
	}
	if bytes.Contains(dict, []byte("/Type /ObjStm")) || bytes.Contains(dict, []byte("/Type/ObjStm")) {
		return true
	}
	if bytes.Contains(dict, []byte("/Type /Metadata")) || bytes.Contains(dict, []byte("/Type/Metadata")) {
		return true
	}
	return false
}

func pdfDecode(s pdfStream) ([]byte, error) {
	data := s.data
	m := rePDFFilter.FindSubmatch(s.dict)
	if len(m) != 2 {
		return data, nil
	}
	names := rePDFFilterNm.FindAll(m[1], -1)
	// PDF applies filters in order; undo them last-to-first.
	for i := len(names) - 1; i >= 0; i-- {
		switch string(names[i]) {
		case "/FlateDecode":
			out, err := inflate(data)
			if err != nil {
				return nil, err
			}
			data = out
		case "/ASCIIHexDecode":
			data = asciiHexDecode(data)
		case "/ASCII85Decode":
			return nil, nil // rare in page content; skip this stream
		case "/DCTDecode", "/JPXDecode", "/JBIG2Decode", "/CCITTFaxDecode":
			return nil, nil
		}
	}
	return data, nil
}

func inflate(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err == nil {
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err == nil {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	defer fr.Close()
	return io.ReadAll(fr)
}

func asciiHexDecode(data []byte) []byte {
	var out []byte
	var nibble int = -1
	for _, c := range data {
		if c == '>' {
			break
		}
		if c <= ' ' {
			continue
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		default:
			continue
		}
		if nibble < 0 {
			nibble = int(v)
		} else {
			out = append(out, byte(nibble<<4)|v)
			nibble = -1
		}
	}
	if nibble >= 0 {
		out = append(out, byte(nibble<<4))
	}
	return out
}

type pdfTok struct {
	kind byte // s string, o operator, a array-of-strings joined
	s    string
}

func pdfContentText(stream []byte) string {
	toks := tokenizePDF(stream)
	hasOp := false
	var b strings.Builder
	space := false
	emit := func(s string, nl bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if nl {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			space = false
		} else if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
		space = true
	}
	for i, tok := range toks {
		if tok.kind != 'o' {
			continue
		}
		switch tok.s {
		case "Tj":
			hasOp = true
			if i > 0 && toks[i-1].kind == 's' {
				emit(toks[i-1].s, false)
			}
		case "'", "\"":
			hasOp = true
			if i > 0 && toks[i-1].kind == 's' {
				emit(toks[i-1].s, true)
			}
		case "TJ":
			hasOp = true
			if i > 0 && (toks[i-1].kind == 'a' || toks[i-1].kind == 's') {
				emit(toks[i-1].s, false)
			}
		case "T*":
			hasOp = true
			if b.Len() > 0 {
				b.WriteByte('\n')
				space = false
			}
		}
	}
	if !hasOp {
		return ""
	}
	return strings.TrimSpace(b.String())
}

func tokenizePDF(stream []byte) []pdfTok {
	var toks []pdfTok
	i := 0
	for i < len(stream) {
		for i < len(stream) && stream[i] <= ' ' {
			i++
		}
		if i >= len(stream) {
			break
		}
		switch stream[i] {
		case '%':
			if j := bytes.IndexByte(stream[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(stream)
			}
		case '(':
			s, n := readPDFLiteral(stream[i:])
			toks = append(toks, pdfTok{kind: 's', s: s})
			i += n
		case '<':
			if i+1 < len(stream) && stream[i+1] == '<' {
				i += 2
				continue
			}
			end := bytes.IndexByte(stream[i:], '>')
			if end < 0 {
				i = len(stream)
				break
			}
			toks = append(toks, pdfTok{kind: 's', s: decodePDFHex(stream[i+1 : i+end])})
			i += end + 1
		case '[':
			inner, n := readPDFArray(stream[i:])
			toks = append(toks, pdfTok{kind: 'a', s: inner})
			i += n
		case '/':
			j := i + 1
			for j < len(stream) && isPDFNameChar(stream[j]) {
				j++
			}
			i = j
		case ']':
			i++
		case '{':
			if j := bytes.IndexByte(stream[i:], '}'); j >= 0 {
				i += j + 1
			} else {
				i++
			}
		default:
			if stream[i] == '-' || stream[i] == '+' || stream[i] == '.' || unicode.IsDigit(rune(stream[i])) {
				j := i + 1
				for j < len(stream) && (stream[j] == '.' || stream[j] == '-' || unicode.IsDigit(rune(stream[j]))) {
					j++
				}
				i = j
				continue
			}
			j := i
			for j < len(stream) && isPDFOpChar(stream[j]) {
				j++
			}
			if j == i {
				i++
				continue
			}
			toks = append(toks, pdfTok{kind: 'o', s: string(stream[i:j])})
			i = j
		}
	}
	return toks
}

func readPDFLiteral(data []byte) (string, int) {
	if len(data) == 0 || data[0] != '(' {
		return "", 0
	}
	var b strings.Builder
	depth := 0
	i := 0
	for i < len(data) {
		c := data[i]
		if c == '\\' && i+1 < len(data) {
			n, adv := pdfEscape(data[i+1:])
			b.WriteString(n)
			i += 1 + adv
			continue
		}
		if c == '(' {
			depth++
			if depth > 1 {
				b.WriteByte(c)
			}
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return b.String(), i + 1
			}
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), i
}

func pdfEscape(data []byte) (string, int) {
	if len(data) == 0 {
		return "", 0
	}
	switch data[0] {
	case 'n':
		return "\n", 1
	case 'r':
		return "\r", 1
	case 't':
		return "\t", 1
	case 'b':
		return "\b", 1
	case 'f':
		return "\f", 1
	case '(', ')', '\\':
		return string(data[0]), 1
	case '\n':
		return "", 1
	case '\r':
		if len(data) > 1 && data[1] == '\n' {
			return "", 2
		}
		return "", 1
	}
	if data[0] >= '0' && data[0] <= '7' {
		n := 0
		adv := 0
		for adv < 3 && adv < len(data) && data[adv] >= '0' && data[adv] <= '7' {
			n = n*8 + int(data[adv]-'0')
			adv++
		}
		return string(rune(n)), adv
	}
	return string(data[0]), 1
}

func readPDFArray(data []byte) (string, int) {
	if len(data) == 0 || data[0] != '[' {
		return "", 0
	}
	depth := 0
	i := 0
	var parts []string
	for i < len(data) {
		c := data[i]
		if c == '(' {
			s, n := readPDFLiteral(data[i:])
			parts = append(parts, s)
			i += n
			continue
		}
		if c == '<' && (i+1 >= len(data) || data[i+1] != '<') {
			end := bytes.IndexByte(data[i:], '>')
			if end < 0 {
				break
			}
			parts = append(parts, decodePDFHex(data[i+1:i+end]))
			i += end + 1
			continue
		}
		if c == '[' {
			depth++
			i++
			continue
		}
		if c == ']' {
			depth--
			i++
			if depth == 0 {
				return strings.Join(parts, ""), i
			}
			continue
		}
		i++
	}
	return strings.Join(parts, ""), i
}

func decodePDFHex(raw []byte) string {
	b := asciiHexDecode(raw)
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		u := make([]uint16, 0, (len(b)-2)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

func isPDFNameChar(c byte) bool {
	return c > ' ' && c != '(' && c != ')' && c != '<' && c != '>' && c != '[' && c != ']' && c != '{' && c != '}' && c != '/' && c != '%'
}

func isPDFOpChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '*' || c == '\'' || c == '"'
}

func indexWord(data []byte, from int, word []byte) int {
	for {
		j := bytes.Index(data[from:], word)
		if j < 0 {
			return -1
		}
		abs := from + j
		if abs > 0 {
			prev := data[abs-1]
			if prev == '/' || (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') {
				from = abs + len(word)
				continue
			}
		}
		end := abs + len(word)
		if end < len(data) {
			next := data[end]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') {
				from = abs + len(word)
				continue
			}
		}
		return abs
	}
}
