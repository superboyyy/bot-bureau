package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func extractDOCX(data []byte) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, err
	}
	var parts []string
	for _, f := range zr.File {
		n := f.Name
		if n != "word/document.xml" && !strings.HasPrefix(n, "word/header") && !strings.HasPrefix(n, "word/footer") {
			continue
		}
		if !strings.HasSuffix(n, ".xml") {
			continue
		}
		body, err := readZip(f)
		if err != nil {
			continue
		}
		if t := wordXMLText(body); strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		return Result{Kind: DOCX}, ErrNoText
	}
	return Result{Kind: DOCX, Text: text}, nil
}

func extractPPTX(data []byte) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, err
	}
	type slide struct {
		n    int
		name string
		file *zip.File
	}
	reSlide := regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	var slides []slide
	for _, f := range zr.File {
		m := reSlide.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		slides = append(slides, slide{n: n, name: f.Name, file: f})
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })
	var b strings.Builder
	found := false
	for _, s := range slides {
		body, err := readZip(s.file)
		if err != nil {
			continue
		}
		t := strings.TrimSpace(wordXMLText(body))
		if t == "" {
			continue
		}
		found = true
		fmt.Fprintf(&b, "--- slide %d ---\n%s\n", s.n, t)
	}
	if !found {
		return Result{Kind: PPTX, Pages: len(slides)}, ErrNoText
	}
	return Result{Kind: PPTX, Pages: len(slides), Text: strings.TrimSpace(b.String())}, nil
}

func extractXLSX(data []byte) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, err
	}
	shared := xlsxSharedStrings(zr)
	type sheet struct {
		n    int
		name string
		file *zip.File
	}
	reSheet := regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)
	var sheets []sheet
	for _, f := range zr.File {
		m := reSheet.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		sheets = append(sheets, sheet{n: n, name: path.Base(f.Name), file: f})
	}
	sort.Slice(sheets, func(i, j int) bool { return sheets[i].n < sheets[j].n })
	names := xlsxSheetNames(zr)
	var b strings.Builder
	found := false
	for i, s := range sheets {
		body, err := readZip(s.file)
		if err != nil {
			continue
		}
		t := strings.TrimSpace(xlsxSheetText(body, shared))
		if t == "" {
			continue
		}
		found = true
		label := names[i]
		if label == "" {
			label = fmt.Sprintf("sheet %d", s.n)
		}
		fmt.Fprintf(&b, "--- sheet %s ---\n%s\n", label, t)
	}
	if !found {
		return Result{Kind: XLSX}, ErrNoText
	}
	return Result{Kind: XLSX, Text: strings.TrimSpace(b.String())}, nil
}

func wordXMLText(raw []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var b strings.Builder
	var inT bool
	paraHas := false
	firstPara := true
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				if paraHas && !firstPara {
					b.WriteByte('\n')
				} else if paraHas {
					firstPara = false
				}
				paraHas = false
			case "t", "instrText":
				inT = true
			case "tab":
				b.WriteByte('\t')
				paraHas = true
			case "br":
				b.WriteByte('\n')
				paraHas = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" || t.Name.Local == "instrText" {
				inT = false
			}
			if t.Name.Local == "p" && paraHas {
				firstPara = false
			}
		case xml.CharData:
			if inT {
				b.Write([]byte(t))
				paraHas = true
			}
		}
	}
	return b.String()
}

func xlsxSharedStrings(zr *zip.Reader) []string {
	var f *zip.File
	for _, file := range zr.File {
		if file.Name == "xl/sharedStrings.xml" {
			f = file
			break
		}
	}
	if f == nil {
		return nil
	}
	body, err := readZip(f)
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out []string
	var cur strings.Builder
	var inSI, inT bool
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				if inSI {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				out = append(out, cur.String())
				inSI = false
			}
		case xml.CharData:
			if inT {
				cur.Write([]byte(t))
			}
		}
	}
	return out
}

func xlsxSheetNames(zr *zip.Reader) []string {
	var f *zip.File
	for _, file := range zr.File {
		if file.Name == "xl/workbook.xml" {
			f = file
			break
		}
	}
	if f == nil {
		return nil
	}
	body, err := readZip(f)
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var names []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sheet" {
			continue
		}
		name := ""
		for _, a := range se.Attr {
			if a.Name.Local == "name" {
				name = a.Value
				break
			}
		}
		names = append(names, name)
	}
	return names
}

func xlsxSheetText(raw []byte, shared []string) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var b strings.Builder
	var row []string
	var cellType, cellVal string
	var inV, inIS, inT bool
	firstRow := true
	flushRow := func() {
		for len(row) > 0 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		if len(row) == 0 {
			return
		}
		if !firstRow {
			b.WriteByte('\n')
		}
		firstRow = false
		b.WriteString(strings.Join(row, "\t"))
		row = nil
	}
	flushCell := func() {
		v := strings.TrimSpace(cellVal)
		if cellType == "s" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(shared) {
				v = shared[n]
			}
		}
		row = append(row, v)
		cellType, cellVal = "", ""
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = nil
			case "c":
				cellType, cellVal = "", ""
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				if inIS {
					inT = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "t":
				inT = false
			case "is":
				inIS = false
			case "c":
				flushCell()
			case "row":
				flushRow()
			}
		case xml.CharData:
			if inV || inT {
				cellVal += string(t)
			}
		}
	}
	return b.String()
}

func readZip(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
