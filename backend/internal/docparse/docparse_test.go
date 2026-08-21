package docparse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestExtractPDFUncompressedAndFlate(t *testing.T) {
	for _, pdf := range [][]byte{
		FixturePDF("Hello PDF"),
		FixturePDFFlate("Hello PDF"),
	} {
		if Detect("note.pdf", pdf) != PDF {
			t.Fatal("Detect should see a PDF")
		}
		res, err := Extract("note.pdf", pdf)
		if err != nil {
			t.Fatal(err)
		}
		if res.Kind != PDF || res.Pages != 1 {
			t.Fatalf("meta: %+v", res)
		}
		if !strings.Contains(res.Text, "Hello PDF") {
			t.Fatalf("text: %q", res.Text)
		}
	}
}

func TestExtractPDFPages(t *testing.T) {
	pdf := FixturePDF("alpha token", "beta token")
	res, err := Extract("two.pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages != 2 {
		t.Fatalf("pages: %d", res.Pages)
	}
	if !strings.Contains(res.Text, "--- page 1 ---") || !strings.Contains(res.Text, "alpha token") {
		t.Fatalf("page 1 missing: %q", res.Text)
	}
	if !strings.Contains(res.Text, "--- page 2 ---") || !strings.Contains(res.Text, "beta token") {
		t.Fatalf("page 2 missing: %q", res.Text)
	}
}

func TestExtractPDFNoText(t *testing.T) {
	// A catalog-only stub is not a valid page stream with Tj; empty pages still
	// produce a content stream of "BT ... Td () Tj ET", which is empty text.
	pdf := FixturePDF("")
	_, err := Extract("empty.pdf", pdf)
	if !errors.Is(err, ErrNoText) {
		t.Fatalf("empty page should be ErrNoText, got %v", err)
	}
}

func TestExtractPDFEncrypted(t *testing.T) {
	pdf := bytes.Replace(FixturePDF("secret"), []byte("/Root 1 0 R >>"), []byte("/Root 1 0 R /Encrypt 9 0 R >>"), 1)
	_, err := Extract("locked.pdf", pdf)
	if !errors.Is(err, ErrEncrypted) {
		t.Fatalf("want ErrEncrypted, got %v", err)
	}
}

func TestExtractOffice(t *testing.T) {
	docx := FixtureDOCX("Quote line", "Second para")
	res, err := Extract("q.docx", docx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != DOCX || !strings.Contains(res.Text, "Quote line") || !strings.Contains(res.Text, "Second para") {
		t.Fatalf("docx: %+v", res)
	}

	xlsx := FixtureXLSX("Prices", [][]string{{"item", "cost"}, {"widget", "9"}})
	res, err = Extract("p.xlsx", xlsx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != XLSX || !strings.Contains(res.Text, "--- sheet Prices ---") {
		t.Fatalf("xlsx sheet label: %+v", res)
	}
	if !strings.Contains(res.Text, "item") || !strings.Contains(res.Text, "widget") || !strings.Contains(res.Text, "9") {
		t.Fatalf("xlsx cells: %q", res.Text)
	}

	pptx := FixturePPTX("Agenda", "Thanks")
	res, err = Extract("d.pptx", pptx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != PPTX || res.Pages != 2 {
		t.Fatalf("pptx meta: %+v", res)
	}
	if !strings.Contains(res.Text, "--- slide 1 ---") || !strings.Contains(res.Text, "Agenda") {
		t.Fatalf("pptx: %q", res.Text)
	}
}

func TestDetectRejectsPlainAndBinary(t *testing.T) {
	if k := Detect("a.txt", []byte("hello")); k != "" {
		t.Fatalf("text: %q", k)
	}
	if k := Detect("a.bin", []byte{0, 1, 2, 0}); k != "" {
		t.Fatalf("bin: %q", k)
	}
	_, err := Extract("a.txt", []byte("hello"))
	if !errors.Is(err, ErrNotDocument) {
		t.Fatalf("not a document: %v", err)
	}
}

func TestTJArrayAndParens(t *testing.T) {
	stream := []byte("BT /F1 12 Tf 72 720 Td [(Hello) -20 (PDF)] TJ ET")
	got := pdfContentText(stream)
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "PDF") {
		t.Fatalf("TJ: %q", got)
	}
	stream = []byte("BT (nested \\(yes\\) ok) Tj ET")
	got = pdfContentText(stream)
	if !strings.Contains(got, "nested (yes) ok") {
		t.Fatalf("literal: %q", got)
	}
}
