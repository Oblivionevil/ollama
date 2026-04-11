//go:build windows || darwin

package ui

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	stdhtml "html"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
)

var structuredWhitespacePattern = regexp.MustCompile(`[ \f\v]+`)

// convertBytesToText converts raw file bytes to text based on file extension
func convertBytesToText(data []byte, filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))

	switch ext {
	case ".pdf":
		text, err := extractPDFText(data)
		return formatExtractedText("PDF", len(data), text, err)
	case ".docx", ".docm":
		text, err := extractOpenXMLText(data, []string{
			"word/document.xml",
			"word/header*.xml",
			"word/footer*.xml",
			"word/footnotes.xml",
			"word/endnotes.xml",
			"word/comments.xml",
		})
		return formatExtractedText(strings.TrimPrefix(strings.ToUpper(ext), "."), len(data), text, err)
	case ".pptx", ".pptm":
		text, err := extractOpenXMLText(data, []string{
			"ppt/slides/slide*.xml",
			"ppt/notesSlides/notesSlide*.xml",
		})
		return formatExtractedText(strings.TrimPrefix(strings.ToUpper(ext), "."), len(data), text, err)
	case ".xlsx", ".xlsm":
		text, err := extractXLSXText(data)
		return formatExtractedText(strings.TrimPrefix(strings.ToUpper(ext), "."), len(data), text, err)
	case ".odt", ".ods", ".odp":
		text, err := extractOpenDocumentText(data)
		return formatExtractedText(strings.TrimPrefix(strings.ToUpper(ext), "."), len(data), text, err)
	case ".rtf":
		text, err := extractRTFText(data)
		return formatExtractedText("RTF", len(data), text, err)
	case ".html", ".htm", ".xhtml":
		text, err := extractHTMLText(data)
		return formatExtractedText("HTML", len(data), text, err)
	}

	binaryExtensions := []string{
		".doc", ".xls", ".ppt", ".zip", ".tar", ".gz", ".rar",
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".ico",
		".mp3", ".mp4", ".avi", ".mov", ".wmv", ".flv", ".webm",
		".exe", ".dll", ".so", ".dylib", ".app", ".dmg", ".pkg",
	}

	if slices.Contains(binaryExtensions, ext) {
		return fmt.Sprintf("[Binary file of type %s - %d bytes]", ext, len(data))
	}

	if utf8.Valid(data) {
		return string(data)
	}

	// If not valid UTF-8, return a placeholder
	return fmt.Sprintf("[Binary file - %d bytes - not valid UTF-8]", len(data))
}

func formatExtractedText(fileType string, size int, text string, err error) string {
	if err != nil {
		return fmt.Sprintf("[%s file - %d bytes - failed to extract text: %v]", fileType, size, err)
	}

	text = normalizeStructuredText(text)
	if text == "" {
		return fmt.Sprintf("[%s file - %d bytes - no text content found]", fileType, size)
	}

	return text
}

func normalizeStructuredText(text string) string {
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	normalized := make([]string, 0, len(lines))
	previousBlank := true

	for _, line := range lines {
		line = structuredWhitespacePattern.ReplaceAllString(strings.TrimSpace(line), " ")
		if line == "" {
			if previousBlank {
				continue
			}
			normalized = append(normalized, "")
			previousBlank = true
			continue
		}

		normalized = append(normalized, line)
		previousBlank = false
	}

	return strings.TrimSpace(strings.Join(normalized, "\n"))
}

func extractOpenXMLText(data []byte, patterns []string) (string, error) {
	reader, err := openZipReader(data)
	if err != nil {
		return "", err
	}

	matchedFiles := matchingZipFiles(reader, patterns)
	if len(matchedFiles) == 0 {
		return "", fmt.Errorf("no readable XML parts found")
	}

	parts := make([]string, 0, len(matchedFiles))
	for _, fileName := range matchedFiles {
		fileData, err := readZipFile(reader, fileName)
		if err != nil {
			return "", err
		}

		text, err := extractXMLText(fileData)
		if err != nil {
			return "", fmt.Errorf("extract %s: %w", fileName, err)
		}

		text = normalizeStructuredText(text)
		if text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

func extractOpenDocumentText(data []byte) (string, error) {
	reader, err := openZipReader(data)
	if err != nil {
		return "", err
	}

	fileData, err := readZipFile(reader, "content.xml")
	if err != nil {
		return "", err
	}

	return extractXMLText(fileData)
}

func extractXLSXText(data []byte) (string, error) {
	reader, err := openZipReader(data)
	if err != nil {
		return "", err
	}

	sharedStrings := []string{}
	if sharedStringsData, err := readZipFile(reader, "xl/sharedStrings.xml"); err == nil {
		sharedStrings, err = extractXLSXSharedStrings(sharedStringsData)
		if err != nil {
			return "", err
		}
	}

	worksheetFiles := matchingZipFiles(reader, []string{"xl/worksheets/sheet*.xml"})
	if len(worksheetFiles) == 0 {
		return "", fmt.Errorf("no worksheets found")
	}

	parts := make([]string, 0, len(worksheetFiles))
	for _, worksheetFile := range worksheetFiles {
		worksheetData, err := readZipFile(reader, worksheetFile)
		if err != nil {
			return "", err
		}

		text, err := extractXLSXWorksheetText(worksheetData, sharedStrings)
		if err != nil {
			return "", fmt.Errorf("extract %s: %w", worksheetFile, err)
		}

		text = normalizeStructuredText(text)
		if text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

func extractXLSXSharedStrings(data []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	stringsList := []string{}
	var current strings.Builder
	inItem := false
	inText := false

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "si":
				current.Reset()
				inItem = true
			case "t":
				inText = true
			case "tab":
				if inItem {
					current.WriteByte('\t')
				}
			case "br":
				if inItem {
					current.WriteByte('\n')
				}
			}
		case xml.EndElement:
			switch typed.Name.Local {
			case "t":
				inText = false
			case "si":
				stringsList = append(stringsList, normalizeStructuredText(current.String()))
				inItem = false
			}
		case xml.CharData:
			if inItem && inText {
				current.Write([]byte(typed))
			}
		}
	}

	return stringsList, nil
}

func extractXLSXWorksheetText(data []byte, sharedStrings []string) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var builder strings.Builder
	rowValues := []string{}
	var cellValue strings.Builder
	currentCellType := ""
	inValue := false
	inText := false

	flushRow := func() {
		if len(rowValues) == 0 {
			return
		}
		builder.WriteString(strings.Join(rowValues, "\t"))
		builder.WriteByte('\n')
		rowValues = rowValues[:0]
	}

	resolveCellValue := func() string {
		value := strings.TrimSpace(cellValue.String())
		if value == "" {
			return ""
		}

		switch currentCellType {
		case "s":
			idx, err := strconv.Atoi(value)
			if err == nil && idx >= 0 && idx < len(sharedStrings) {
				return sharedStrings[idx]
			}
		}

		return value
	}

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "row":
				rowValues = rowValues[:0]
			case "c":
				currentCellType = ""
				cellValue.Reset()
				for _, attr := range typed.Attr {
					if attr.Name.Local == "t" {
						currentCellType = attr.Value
						break
					}
				}
			case "v":
				inValue = true
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch typed.Name.Local {
			case "v":
				inValue = false
			case "t":
				inText = false
			case "c":
				if value := resolveCellValue(); value != "" {
					rowValues = append(rowValues, value)
				}
				cellValue.Reset()
				currentCellType = ""
			case "row":
				flushRow()
			}
		case xml.CharData:
			if inValue || inText {
				cellValue.Write([]byte(typed))
			}
		}
	}

	flushRow()
	return builder.String(), nil
}

func extractRTFText(data []byte) (string, error) {
	decoded := data
	if !utf8.Valid(decoded) {
		utf8Data, err := charmap.Windows1252.NewDecoder().Bytes(decoded)
		if err == nil {
			decoded = utf8Data
		}
	}

	text := string(decoded)
	var builder strings.Builder

	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{', '}':
			continue
		case '\\':
			if i+1 >= len(text) {
				continue
			}

			next := text[i+1]
			switch next {
			case '\\', '{', '}':
				builder.WriteByte(next)
				i++
				continue
			case '\'':
				if i+3 >= len(text) {
					continue
				}
				hexValue := text[i+2 : i+4]
				value, err := strconv.ParseUint(hexValue, 16, 8)
				if err == nil {
					decodedByte, decodeErr := charmap.Windows1252.NewDecoder().Bytes([]byte{byte(value)})
					if decodeErr == nil {
						builder.Write(decodedByte)
					}
				}
				i += 3
				continue
			}

			j := i + 1
			for j < len(text) && ((text[j] >= 'a' && text[j] <= 'z') || (text[j] >= 'A' && text[j] <= 'Z')) {
				j++
			}

			controlWord := text[i+1 : j]
			paramStart := j
			if j < len(text) && (text[j] == '-' || (text[j] >= '0' && text[j] <= '9')) {
				j++
				for j < len(text) && text[j] >= '0' && text[j] <= '9' {
					j++
				}
			}
			parameter := text[paramStart:j]

			if j < len(text) && text[j] == ' ' {
				i = j
			} else {
				i = j - 1
			}

			switch controlWord {
			case "par", "line":
				builder.WriteByte('\n')
			case "tab":
				builder.WriteByte('\t')
			case "emdash":
				builder.WriteRune('—')
			case "endash":
				builder.WriteRune('–')
			case "u":
				value, err := strconv.Atoi(parameter)
				if err == nil {
					if value < 0 {
						value += 65536
					}
					builder.WriteRune(rune(value))
					if i+1 < len(text) && text[i+1] != '\\' && text[i+1] != '{' && text[i+1] != '}' {
						i++
					}
				}
			}
		default:
			builder.WriteByte(text[i])
		}
	}

	return builder.String(), nil
}

func extractHTMLText(data []byte) (string, error) {
	decoded := data
	if !utf8.Valid(decoded) {
		utf8Data, err := charmap.Windows1252.NewDecoder().Bytes(decoded)
		if err == nil {
			decoded = utf8Data
		}
	}

	doc, err := html.Parse(bytes.NewReader(decoded))
	if err != nil {
		return "", err
	}

	blockElements := map[string]struct{}{
		"article": {}, "blockquote": {}, "br": {}, "div": {}, "h1": {}, "h2": {}, "h3": {},
		"h4": {}, "h5": {}, "h6": {}, "header": {}, "footer": {}, "li": {}, "main": {},
		"p": {}, "section": {}, "table": {}, "tr": {}, "ul": {}, "ol": {},
	}

	var builder strings.Builder
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, skip bool) {
		if node == nil {
			return
		}

		if node.Type == html.ElementNode {
			name := strings.ToLower(node.Data)
			if name == "script" || name == "style" || name == "noscript" {
				skip = true
			}
			if !skip {
				if _, ok := blockElements[name]; ok {
					builder.WriteByte('\n')
				}
			}
		}

		if node.Type == html.TextNode && !skip {
			builder.WriteString(stdhtml.UnescapeString(node.Data))
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, skip)
		}

		if node.Type == html.ElementNode && !skip {
			if _, ok := blockElements[strings.ToLower(node.Data)]; ok {
				builder.WriteByte('\n')
			}
		}
	}

	walk(doc, false)
	return builder.String(), nil
}

func openZipReader(data []byte) (*zip.Reader, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	return reader, nil
}

func matchingZipFiles(reader *zip.Reader, patterns []string) []string {
	matches := make([]string, 0)
	for _, file := range reader.File {
		for _, pattern := range patterns {
			matched, err := path.Match(pattern, file.Name)
			if err == nil && matched {
				matches = append(matches, file.Name)
				break
			}
		}
	}

	sort.Strings(matches)
	return matches
}

func readZipFile(reader *zip.Reader, name string) ([]byte, error) {
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		defer rc.Close()

		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return data, nil
	}

	return nil, fmt.Errorf("archive entry %s not found", name)
}

func extractXMLText(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var builder strings.Builder

	blockBreaks := map[string]struct{}{
		"div": {}, "h1": {}, "h2": {}, "h3": {}, "h4": {}, "h5": {}, "h6": {},
		"li": {}, "p": {}, "row": {}, "section": {}, "sheet": {}, "sheetData": {},
		"si": {}, "slide": {}, "table-row": {}, "tr": {}, "txBody": {}, "worksheet": {},
	}

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "br", "cr", "line-break":
				builder.WriteByte('\n')
			case "tab":
				builder.WriteByte('\t')
			}
		case xml.EndElement:
			if _, ok := blockBreaks[typed.Name.Local]; ok {
				builder.WriteByte('\n')
			}
		case xml.CharData:
			if strings.TrimSpace(string(typed)) == "" {
				continue
			}
			builder.Write([]byte(typed))
		}
	}

	return builder.String(), nil
}

// extractPDFText extracts text content from PDF bytes
func extractPDFText(data []byte) (string, error) {
	reader := bytes.NewReader(data)
	pdfReader, err := pdf.NewReader(reader, int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to create PDF reader: %w", err)
	}

	var textBuilder strings.Builder
	numPages := pdfReader.NumPage()

	for i := 1; i <= numPages; i++ {
		page := pdfReader.Page(i)
		if page.V.IsNull() {
			continue
		}

		text, err := page.GetPlainText(nil)
		if err != nil {
			// Log the error but continue with other pages
			continue
		}

		if strings.TrimSpace(text) != "" {
			if textBuilder.Len() > 0 {
				textBuilder.WriteString("\n\n--- Page ")
				textBuilder.WriteString(fmt.Sprintf("%d", i))
				textBuilder.WriteString(" ---\n")
			}
			textBuilder.WriteString(text)
		}
	}

	return textBuilder.String(), nil
}
