package docparse

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
)

// FixturePDF returns a minimal PDF whose pages contain the given Helvetica strings.
func FixturePDF(pages ...string) []byte {
	return buildPDF(pages, false)
}

// FixturePDFFlate is FixturePDF with FlateDecode on each content stream.
func FixturePDFFlate(pages ...string) []byte {
	return buildPDF(pages, true)
}

func buildPDF(pages []string, flate bool) []byte {
	if len(pages) == 0 {
		pages = []string{""}
	}
	n := len(pages)
	fontObj := 3 + n
	firstContent := fontObj + 1
	kids := make([]string, n)
	for i := 0; i < n; i++ {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i)
	}
	bodies := make([]string, 0, 2+2*n+1)
	bodies = append(bodies, "<< /Type /Catalog /Pages 2 0 R >>")
	bodies = append(bodies, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for i := 0; i < n; i++ {
		bodies = append(bodies, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>",
			firstContent+i, fontObj))
	}
	bodies = append(bodies, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for _, t := range pages {
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", pdfStringEscape(t))
		if flate {
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			_, _ = zw.Write([]byte(stream))
			_ = zw.Close()
			bodies = append(bodies, fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", buf.Len(), buf.Bytes()))
			continue
		}
		bodies = append(bodies, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(bodies))
	for i, body := range bodies {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(bodies)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(bodies)+1, xref)
	return buf.Bytes()
}

func pdfStringEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "(", "\\(")
	s = strings.ReplaceAll(s, ")", "\\)")
	return s
}

// FixtureDOCX returns a .docx whose body is the given paragraphs.
func FixtureDOCX(paragraphs ...string) []byte {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>`)
		body.WriteString(xmlEscape(p))
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	body.WriteString(`</w:body></w:document>`)
	return zipBytes(map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		"word/document.xml":   body.String(),
		"_rels/.rels":         `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
	})
}

// FixtureXLSX returns a .xlsx with one sheet and the given rows (cells are strings).
func FixtureXLSX(sheet string, rows [][]string) []byte {
	if sheet == "" {
		sheet = "Sheet1"
	}
	var shared []string
	idx := map[string]int{}
	index := func(s string) int {
		if n, ok := idx[s]; ok {
			return n
		}
		n := len(shared)
		shared = append(shared, s)
		idx[s] = n
		return n
	}
	var sheetXML strings.Builder
	sheetXML.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sheetXML.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sheetXML, `<row r="%d">`, r+1)
		for c, cell := range row {
			fmt.Fprintf(&sheetXML, `<c r="%s%d" t="s"><v>%d</v></c>`, colName(c), r+1, index(cell))
		}
		sheetXML.WriteString(`</row>`)
	}
	sheetXML.WriteString(`</sheetData></worksheet>`)
	var ss strings.Builder
	ss.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	ss.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, s := range shared {
		ss.WriteString(`<si><t>`)
		ss.WriteString(xmlEscape(s))
		ss.WriteString(`</t></si>`)
	}
	ss.WriteString(`</sst>`)
	wb := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="%s" sheetId="1" r:id="rId1"/></sheets></workbook>`, xmlEscape(sheet))
	return zipBytes(map[string]string{
		"[Content_Types].xml":        `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		"xl/workbook.xml":            wb,
		"xl/sharedStrings.xml":       ss.String(),
		"xl/worksheets/sheet1.xml":   sheetXML.String(),
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
	})
}

// FixturePPTX returns a .pptx whose slides contain the given titles.
func FixturePPTX(slides ...string) []byte {
	files := map[string]string{
		"[Content_Types].xml":  `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
		"ppt/presentation.xml": `<?xml version="1.0" encoding="UTF-8"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"></p:presentation>`,
	}
	for i, title := range slides {
		files[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = fmt.Sprintf(
			`<?xml version="1.0" encoding="UTF-8"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>%s</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
			xmlEscape(title),
		)
	}
	return zipBytes(files)
}

func colName(i int) string {
	s := ""
	for i >= 0 {
		s = string(rune('A'+i%26)) + s
		i = i/26 - 1
	}
	return s
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func zipBytes(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
