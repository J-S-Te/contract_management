package docx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

const MaxTemplateSize = 10 << 20

var (
	textNodePattern    = regexp.MustCompile(`(?s)<w:t\b[^>]*>(.*?)</w:t>`)
	paragraphPattern   = regexp.MustCompile(`(?s)<w:p\b[^>]*>.*?</w:p>`)
	placeholderPattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
	defaultPattern     = regexp.MustCompile(`^(.+?)\s+(?:'([^']*)'|"([^"]*)")$`)
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
	fields := map[string]contracttemplate.Field{}
	for name, body := range files {
		if !isWordXML(name) {
			continue
		}
		visible, _ := visibleText(string(body))
		for _, match := range placeholderPattern.FindAllStringSubmatch(visible, -1) {
			spec, ok := parsePlaceholder(match[1])
			if !ok {
				continue
			}
			if _, exists := fields[spec.name]; !exists {
				fields[spec.name] = contracttemplate.Field{Name: spec.name, Label: spec.label, Default: spec.defaultValue}
			}
		}
	}
	result := make([]contracttemplate.Field, 0, len(fields))
	for _, field := range fields {
		result = append(result, field)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) == 0 {
		return nil, fmt.Errorf("DOCX 中未找到 {{字段名}} 格式的占位符")
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
	return renderDocumentHTML(body)
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
		rawSpec := visible[match[2]:match[3]]
		spec, renderable := parsePlaceholder(rawSpec)
		if !renderable {
			continue
		}
		value, ok := values[spec.name]
		if !ok && spec.defaultValue != "" {
			value, ok = spec.defaultValue, true
		}
		if !ok {
			return "", fmt.Errorf("缺少模板字段 %q 的值", spec.name)
		}
		if spec.transform == "money_upper" {
			converted, conversionErr := chineseMoneyUpper(value)
			if conversionErr != nil {
				return "", fmt.Errorf("模板字段 %q 无法转换为金额大写：%w", spec.name, conversionErr)
			}
			value = converted
		}
		startNode, startOffset := locate(nodes, match[0], false)
		endNode, endOffset := locate(nodes, match[1], true)
		if startNode < 0 || endNode < 0 {
			return "", fmt.Errorf("无法定位模板字段 %q", spec.name)
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
	if strings.IndexFunc(name, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
		return name
	}
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(name))
	for index := range words {
		words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
	}
	return strings.Join(words, " ")
}

type placeholderSpec struct {
	name         string
	label        string
	defaultValue string
	transform    string
}

func parsePlaceholder(raw string) (placeholderSpec, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "/") || raw == "else" {
		return placeholderSpec{}, false
	}
	if strings.HasPrefix(raw, "金额_大写 ") {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "金额_大写 "))
		if raw == "" {
			return placeholderSpec{}, false
		}
		result := placeholderSpec{name: raw, label: fieldLabel(raw), transform: "money_upper"}
		return result, true
	}
	result := placeholderSpec{name: raw}
	if match := defaultPattern.FindStringSubmatch(raw); match != nil {
		result.name = strings.TrimSpace(match[1])
		result.defaultValue = match[2]
		if result.defaultValue == "" {
			result.defaultValue = match[3]
		}
	}
	if before, after, found := strings.Cut(result.name, ":"); found {
		result.name, result.label = strings.TrimSpace(before), strings.TrimSpace(after)
	}
	if result.name == "" || len(result.name) > 128 {
		return placeholderSpec{}, false
	}
	if result.label == "" {
		result.label = fieldLabel(result.name)
	}
	return result, true
}

func chineseMoneyUpper(raw string) (string, error) {
	normalized := strings.NewReplacer(",", "", "，", "", "￥", "", "¥", "", "元", "", " ", "").Replace(strings.TrimSpace(raw))
	parts := strings.Split(normalized, ".")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" || len(parts) == 2 && len(parts[1]) > 2 {
		return "", fmt.Errorf("金额格式不正确")
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return "", fmt.Errorf("金额只能包含数字和最多两位小数")
			}
		}
	}
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	if len(integer) > 16 {
		return "", fmt.Errorf("金额数值过大")
	}
	result := chineseIntegerUpper(integer) + "元"
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1] + strings.Repeat("0", 2-len(parts[1]))
	}
	digits := []string{"零", "壹", "贰", "叁", "肆", "伍", "陆", "柒", "捌", "玖"}
	if fraction == "" || fraction == "00" {
		return result + "整", nil
	}
	jiao, fen := fraction[0]-'0', fraction[1]-'0'
	if jiao > 0 {
		result += digits[jiao] + "角"
	}
	if fen > 0 {
		if jiao == 0 && integer != "0" {
			result += "零"
		}
		result += digits[fen] + "分"
	}
	return result, nil
}

func chineseIntegerUpper(integer string) string {
	if integer == "0" {
		return "零"
	}
	digits := []string{"零", "壹", "贰", "叁", "肆", "伍", "陆", "柒", "捌", "玖"}
	groupUnits := []string{"", "万", "亿", "兆"}
	groups := make([]int, 0, (len(integer)+3)/4)
	for end := len(integer); end > 0; end -= 4 {
		start := end - 4
		if start < 0 {
			start = 0
		}
		value := 0
		for _, digit := range integer[start:end] {
			value = value*10 + int(digit-'0')
		}
		groups = append(groups, value)
	}
	var result strings.Builder
	zeroPending := false
	for index := len(groups) - 1; index >= 0; index-- {
		group := groups[index]
		if group == 0 {
			if result.Len() > 0 {
				zeroPending = true
			}
			continue
		}
		if result.Len() > 0 && (zeroPending || group < 1000) {
			result.WriteString("零")
		}
		result.WriteString(chineseFourDigits(group, digits))
		result.WriteString(groupUnits[index])
		zeroPending = false
	}
	return result.String()
}

func chineseFourDigits(value int, digits []string) string {
	units := []string{"", "拾", "佰", "仟"}
	divisors := []int{1000, 100, 10, 1}
	var result strings.Builder
	zeroPending := false
	for index, divisor := range divisors {
		digit := value / divisor % 10
		if digit == 0 {
			if result.Len() > 0 && value%divisor != 0 {
				zeroPending = true
			}
			continue
		}
		if zeroPending {
			result.WriteString("零")
			zeroPending = false
		}
		result.WriteString(digits[digit])
		result.WriteString(units[3-index])
	}
	return result.String()
}

var safeHexColor = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)

type officeNode struct {
	name     string
	attrs    map[string]string
	text     string
	children []*officeNode
}

func renderDocumentHTML(documentXML []byte) (string, error) {
	root, err := parseOfficeXML(documentXML)
	if err != nil {
		return "", fmt.Errorf("parse DOCX document XML: %w", err)
	}
	body := descendant(root, "body")
	if body == nil {
		return "", fmt.Errorf("invalid DOCX: document body is missing")
	}
	var result strings.Builder
	result.WriteString(`<article class="docx-preview docx-page">`)
	for _, child := range body.children {
		renderOfficeBlock(&result, child)
	}
	result.WriteString(`</article>`)
	return result.String(), nil
}

func parseOfficeXML(body []byte) (*officeNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	root := &officeNode{name: "root", attrs: map[string]string{}}
	stack := []*officeNode{root}
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			node := &officeNode{name: value.Name.Local, attrs: map[string]string{}}
			for _, attribute := range value.Attr {
				node.attrs[attribute.Name.Local] = attribute.Value
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.CharData:
			stack[len(stack)-1].text += string(value)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return root, nil
}

func renderOfficeBlock(result *strings.Builder, node *officeNode) {
	switch node.name {
	case "p":
		renderParagraph(result, node)
	case "tbl":
		renderTable(result, node)
	case "sdt", "customXml":
		for _, child := range node.children {
			renderOfficeBlock(result, child)
		}
	}
}

func renderParagraph(result *strings.Builder, paragraph *officeNode) {
	properties := child(paragraph, "pPr")
	styles := make([]string, 0, 3)
	classes := []string{"docx-paragraph"}
	if alignment := attr(child(properties, "jc"), "val"); alignment != "" {
		alignments := map[string]string{"center": "center", "right": "right", "both": "justify", "distribute": "justify", "left": "left"}
		if safe, exists := alignments[alignment]; exists {
			styles = append(styles, "text-align:"+safe)
		}
	}
	if style := strings.ToLower(attr(child(properties, "pStyle"), "val")); strings.Contains(style, "title") || strings.Contains(style, "heading") {
		classes = append(classes, "docx-heading")
	}
	if child(properties, "numPr") != nil {
		classes = append(classes, "docx-list-item")
	}
	result.WriteString(`<p class="` + strings.Join(classes, " ") + `"`)
	if len(styles) > 0 {
		result.WriteString(` style="` + strings.Join(styles, ";") + `"`)
	}
	result.WriteString(`>`)
	if child(properties, "numPr") != nil {
		result.WriteString(`<span class="docx-list-marker">•</span>`)
	}
	before := result.Len()
	for _, item := range paragraph.children {
		if item.name != "pPr" {
			renderInline(result, item)
		}
	}
	if result.Len() == before {
		result.WriteString(`&nbsp;`)
	}
	result.WriteString(`</p>`)
}

func renderInline(result *strings.Builder, node *officeNode) {
	switch node.name {
	case "r":
		renderRun(result, node)
	case "t", "delText", "instrText":
		result.WriteString(html.EscapeString(node.text))
	case "tab":
		result.WriteString(`<span class="docx-tab">&#9;</span>`)
	case "br", "cr":
		result.WriteString(`<br>`)
	case "hyperlink", "smartTag", "sdt", "sdtContent", "ins", "fldSimple":
		for _, child := range node.children {
			renderInline(result, child)
		}
	}
}

func renderRun(result *strings.Builder, run *officeNode) {
	properties := child(run, "rPr")
	styles := make([]string, 0, 6)
	if enabled(child(properties, "b")) {
		styles = append(styles, "font-weight:700")
	}
	if enabled(child(properties, "i")) {
		styles = append(styles, "font-style:italic")
	}
	if enabled(child(properties, "u")) {
		styles = append(styles, "text-decoration:underline")
	}
	if size, err := strconv.Atoi(attr(child(properties, "sz"), "val")); err == nil && size >= 12 && size <= 144 {
		styles = append(styles, fmt.Sprintf("font-size:%.1fpt", float64(size)/2))
	}
	if color := attr(child(properties, "color"), "val"); safeHexColor.MatchString(color) {
		styles = append(styles, "color:#"+color)
	}
	vertical := attr(child(properties, "vertAlign"), "val")
	tag := "span"
	if vertical == "superscript" {
		tag = "sup"
	} else if vertical == "subscript" {
		tag = "sub"
	}
	result.WriteString("<" + tag)
	if len(styles) > 0 {
		result.WriteString(` style="` + strings.Join(styles, ";") + `"`)
	}
	result.WriteString(">")
	for _, item := range run.children {
		if item.name != "rPr" {
			renderInline(result, item)
		}
	}
	result.WriteString("</" + tag + ">")
}

func renderTable(result *strings.Builder, table *officeNode) {
	result.WriteString(`<table class="docx-table"><tbody>`)
	for _, row := range table.children {
		if row.name != "tr" {
			continue
		}
		result.WriteString(`<tr>`)
		for _, cell := range row.children {
			if cell.name != "tc" {
				continue
			}
			properties := child(cell, "tcPr")
			result.WriteString(`<td`)
			if span, err := strconv.Atoi(attr(child(properties, "gridSpan"), "val")); err == nil && span > 1 && span <= 32 {
				result.WriteString(fmt.Sprintf(` colspan="%d"`, span))
			}
			if fill := attr(child(properties, "shd"), "fill"); safeHexColor.MatchString(fill) {
				result.WriteString(` style="background-color:#` + fill + `"`)
			}
			result.WriteString(`>`)
			for _, block := range cell.children {
				if block.name != "tcPr" {
					renderOfficeBlock(result, block)
				}
			}
			result.WriteString(`</td>`)
		}
		result.WriteString(`</tr>`)
	}
	result.WriteString(`</tbody></table>`)
}

func child(node *officeNode, name string) *officeNode {
	if node == nil {
		return nil
	}
	for _, item := range node.children {
		if item.name == name {
			return item
		}
	}
	return nil
}

func descendant(node *officeNode, name string) *officeNode {
	if node == nil {
		return nil
	}
	if node.name == name {
		return node
	}
	for _, item := range node.children {
		if found := descendant(item, name); found != nil {
			return found
		}
	}
	return nil
}

func attr(node *officeNode, name string) string {
	if node == nil {
		return ""
	}
	return node.attrs[name]
}

func enabled(node *officeNode) bool {
	if node == nil {
		return false
	}
	value := strings.ToLower(attr(node, "val"))
	return value != "0" && value != "false" && value != "off" && value != "none"
}
