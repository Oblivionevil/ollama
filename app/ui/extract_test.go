//go:build windows || darwin

package ui

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestConvertBytesToTextDOCX(t *testing.T) {
	data := makeArchive(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello DOCX</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p></w:body></w:document>`,
	})

	got := convertBytesToText(data, "sample.docx")
	if !strings.Contains(got, "Hello DOCX") || !strings.Contains(got, "Second paragraph") {
		t.Fatalf("expected extracted DOCX text, got %q", got)
	}
	if strings.Contains(got, "Binary file") {
		t.Fatalf("expected extracted DOCX text instead of binary placeholder, got %q", got)
	}
}

func TestConvertBytesToTextPPTX(t *testing.T) {
	data := makeArchive(t, map[string]string{
		"ppt/slides/slide1.xml": `<?xml version="1.0" encoding="UTF-8"?><p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Quarterly Review</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	})

	got := convertBytesToText(data, "slides.pptx")
	if !strings.Contains(got, "Quarterly Review") {
		t.Fatalf("expected extracted PPTX text, got %q", got)
	}
}

func TestConvertBytesToTextXLSX(t *testing.T) {
	data := makeArchive(t, map[string]string{
		"xl/sharedStrings.xml":     `<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Quarter</t></si><si><t>Revenue</t></si><si><t>Q1</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>1200</v></c></row></sheetData></worksheet>`,
	})

	got := convertBytesToText(data, "report.xlsx")
	if !strings.Contains(got, "Quarter\tRevenue") {
		t.Fatalf("expected extracted XLSX header row, got %q", got)
	}
	if !strings.Contains(got, "Q1\t1200") {
		t.Fatalf("expected extracted XLSX data row, got %q", got)
	}
}

func TestConvertBytesToTextODT(t *testing.T) {
	data := makeArchive(t, map[string]string{
		"content.xml": `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body><office:text><text:p>Hello ODT</text:p><text:p>Another line</text:p></office:text></office:body></office:document-content>`,
	})

	got := convertBytesToText(data, "notes.odt")
	if !strings.Contains(got, "Hello ODT") || !strings.Contains(got, "Another line") {
		t.Fatalf("expected extracted ODT text, got %q", got)
	}
}

func TestConvertBytesToTextRTFAndHTML(t *testing.T) {
	rtf := []byte(`{\rtf1\ansi This is \b bold\b0\par Second line}`)
	html := []byte(`<html><body><h1>Title</h1><p>Hello <strong>HTML</strong></p></body></html>`)

	if got := convertBytesToText(rtf, "sample.rtf"); !strings.Contains(got, "This is bold") || !strings.Contains(got, "Second line") {
		t.Fatalf("expected extracted RTF text, got %q", got)
	}

	if got := convertBytesToText(html, "sample.html"); !strings.Contains(got, "Title") || !strings.Contains(got, "Hello HTML") {
		t.Fatalf("expected extracted HTML text, got %q", got)
	}
}

func makeArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		fileWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := fileWriter.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	return buffer.Bytes()
}
