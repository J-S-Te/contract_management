package docx

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

const MaxTemplateSize = 10 << 20

var (
	textNodePattern    = regexp.MustCompile(`(?s)<w:t\b[^>]*>(.*?)</w:t>`)
	paragraphPattern   = regexp.MustCompile(`(?s)<w:p\b[^>]*>.*?</w:p>`)
	placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z][A-Za-z0-9_]*)(?:\s*:\s*([^{}]+?))?\s*\}\}`)
)

type textNode struct {
	start, end      int
	contentStart    int
	contentEnd      int
	original, value string
	visibleStart    int
	visibleEnd      int
}

func Fields(document []byte) ([]contracttemplate.Field, error) {
	files, err := read(document)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for name, body := range files {
		if !isWordXML(name) {
			continue
		}
		visible, _ := visibleText(string(body))
		for _, match := range placeholderPattern.FindAllStringSubmatch(visible, -1) {
			label := strings.TrimSpace(match[2])
			if label == "" {
				label = fieldLabel(match[1])
			}
			if _, exists := labels[match[1]]; !exists {
				labels[match[1]] = label
			}
		}
	}
	result := make([]contracttemplate.Field, 0, len(labels))
	for name, label := range labels {
		result = append(result, contracttemplate.Field{Name: name, Label: label})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) == 0 {
		return nil, fmt.Errorf("docx contains no {{field_name}} placeholders")
	}
	return result, nil
}

func Render(document []byte, values map[string]string) ([]byte, error) {
	files, err := read(document)
	if err != nil {
		return nil, err
	}
	for name, body := range files {
		if !isWordXML(name) {
			continue
		}
		rendered, err := replacePlaceholders(string(body), values)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		files[name] = []byte(rendered)
	}
	return write(document, files)
}

func PreviewHTML(document []byte) (string, error) {
	files, err := read(document)
	if err != nil {
		return "", err
	}
	body, ok := files["word/document.xml"]
	if !ok {
		return "", fmt.Errorf("invalid docx: word/document.xml is missing")
	}
	paragraphs := paragraphPattern.FindAll(body, -1)
	var result strings.Builder
	result.WriteString(`<article class="docx-preview">`)
	for _, paragraph := range paragraphs {
		text, _ := visibleText(string(paragraph))
		if strings.TrimSpace(text) == "" {
			result.WriteString("<p>&nbsp;</p>")
			continue
		}
		result.WriteString("<p>")
		result.WriteString(html.EscapeString(text))
		result.WriteString("</p>")
	}
	result.WriteString("</article>")
	return result.String(), nil
}

func PlainText(document []byte) (string, error) {
	files, err := read(document)
	if err != nil {
		return "", err
	}
	body, ok := files["word/document.xml"]
	if !ok {
		return "", fmt.Errorf("invalid docx: word/document.xml is missing")
	}
	paragraphs := paragraphPattern.FindAll(body, -1)
	lines := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		text, _ := visibleText(string(paragraph))
		if text = strings.TrimSpace(text); text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func replacePlaceholders(xml string, values map[string]string) (string, error) {
	visible, nodes := visibleText(xml)
	matches := placeholderPattern.FindAllStringSubmatchIndex(visible, -1)
	for index := len(matches) - 1; index >= 0; index-- {
		match := matches[index]
		name := visible[match[2]:match[3]]
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("missing value for %q", name)
		}
		startNode, startOffset := locate(nodes, match[0], false)
		endNode, endOffset := locate(nodes, match[1], true)
		if startNode < 0 || endNode < 0 {
			return "", fmt.Errorf("cannot locate placeholder %q", name)
		}
		if startNode == endNode {
			nodes[startNode].value = nodes[startNode].value[:startOffset] + value + nodes[startNode].value[endOffset:]
			continue
		}
		nodes[startNode].value = nodes[startNode].value[:startOffset] + value
		for nodeIndex := startNode + 1; nodeIndex < endNode; nodeIndex++ {
			nodes[nodeIndex].value = ""
		}
		nodes[endNode].value = nodes[endNode].value[endOffset:]
	}
	var result strings.Builder
	cursor := 0
	for _, node := range nodes {
		result.WriteString(xml[cursor:node.contentStart])
		result.WriteString(html.EscapeString(node.value))
		cursor = node.contentEnd
	}
	result.WriteString(xml[cursor:])
	return result.String(), nil
}

func visibleText(xml string) (string, []textNode) {
	matches := textNodePattern.FindAllStringSubmatchIndex(xml, -1)
	nodes := make([]textNode, 0, len(matches))
	var visible strings.Builder
	for _, match := range matches {
		raw := xml[match[2]:match[3]]
		value := html.UnescapeString(raw)
		start := visible.Len()
		visible.WriteString(value)
		nodes = append(nodes, textNode{
			start: match[0], end: match[1], contentStart: match[2], contentEnd: match[3],
			original: value, value: value, visibleStart: start, visibleEnd: visible.Len(),
		})
	}
	return visible.String(), nodes
}

func locate(nodes []textNode, offset int, end bool) (int, int) {
	for index, node := range nodes {
		if offset >= node.visibleStart && (offset < node.visibleEnd || end && offset == node.visibleEnd) {
			return index, offset - node.visibleStart
		}
	}
	return -1, 0
}

func read(document []byte) (map[string][]byte, error) {
	if len(document) == 0 || len(document) > MaxTemplateSize {
		return nil, fmt.Errorf("docx size must be between 1 byte and %d bytes", MaxTemplateSize)
	}
	reader, err := zip.NewReader(bytes.NewReader(document), int64(len(document)))
	if err != nil {
		return nil, fmt.Errorf("invalid docx zip: %w", err)
	}
	files := make(map[string][]byte, len(reader.File))
	var expanded uint64
	for _, file := range reader.File {
		expanded += file.UncompressedSize64
		if expanded > 50<<20 {
			return nil, fmt.Errorf("docx expanded content is too large")
		}
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		files[file.Name] = body
	}
	if _, ok := files["[Content_Types].xml"]; !ok {
		return nil, fmt.Errorf("invalid docx: [Content_Types].xml is missing")
	}
	if _, ok := files["word/document.xml"]; !ok {
		return nil, fmt.Errorf("invalid docx: word/document.xml is missing")
	}
	return files, nil
}

func write(original []byte, files map[string][]byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(original), int64(len(original)))
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, source := range reader.File {
		header := source.FileHeader
		target, err := writer.CreateHeader(&header)
		if err != nil {
			return nil, err
		}
		if _, err := target.Write(files[source.Name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func isWordXML(name string) bool {
	return strings.HasPrefix(name, "word/") && path.Ext(name) == ".xml"
}

func fieldLabel(name string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(name))
	for index := range words {
		words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
	}
	return strings.Join(words, " ")
}
