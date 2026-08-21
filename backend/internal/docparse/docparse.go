// Package docparse turns PDF and Office files into plain text so members can
// read_file and grep them. The files themselves stay binary; this is a best-effort
// extract, not a round-trip editor.
package docparse

import (
	"archive/zip"
	"bytes"
	"errors"
	"path"
	"strings"
)

var (
	// ErrNotDocument is returned when the bytes are not a PDF or Office file.
	ErrNotDocument = errors.New("not a PDF or Office document")
	// ErrNoText means the file parsed but had nothing extractable (scanned PDF, empty slides).
	ErrNoText = errors.New("no extractable text")
	// ErrEncrypted means the PDF has an /Encrypt dictionary.
	ErrEncrypted = errors.New("encrypted PDF")
)

// Kind is the document type Detect and Extract report.
type Kind string

const (
	PDF  Kind = "PDF"
	DOCX Kind = "Word"
	XLSX Kind = "Excel"
	PPTX Kind = "PowerPoint"
)

// Result is extracted text plus enough context to label it for the model.
type Result struct {
	Kind  Kind
	Pages int // PDF page count; 0 when unknown or not a PDF
	Text  string
}

// Detect classifies by magic bytes, then by the names inside a zip.
func Detect(name string, data []byte) Kind {
	if isPDF(data) {
		return PDF
	}
	if !isZip(data) {
		return ""
	}
	if k := zipKind(data); k != "" {
		return k
	}
	switch strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/"))) {
	case ".docx":
		return DOCX
	case ".xlsx":
		return XLSX
	case ".pptx":
		return PPTX
	}
	return ""
}

// Extract returns plain text for a PDF or Office file.
func Extract(name string, data []byte) (Result, error) {
	switch Detect(name, data) {
	case PDF:
		return extractPDF(data)
	case DOCX:
		return extractDOCX(data)
	case XLSX:
		return extractXLSX(data)
	case PPTX:
		return extractPPTX(data)
	default:
		return Result{}, ErrNotDocument
	}
}

func isPDF(data []byte) bool {
	return len(data) >= 4 && bytes.HasPrefix(data, []byte("%PDF"))
}

func isZip(data []byte) bool {
	return len(data) >= 4 && bytes.HasPrefix(data, []byte("PK\x03\x04"))
}

func zipKind(data []byte) Kind {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var hasWord, hasXL, hasPPT bool
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			hasWord = true
		case "xl/workbook.xml":
			hasXL = true
		case "ppt/presentation.xml":
			hasPPT = true
		}
	}
	switch {
	case hasWord:
		return DOCX
	case hasXL:
		return XLSX
	case hasPPT:
		return PPTX
	}
	return ""
}
