package docx

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFieldsAndRenderPlaceholderSplitAcrossWordRuns(t *testing.T) {
	document := testDocument(t, `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>客户：</w:t></w:r><w:r><w:t>{{customer_</w:t></w:r><w:r><w:t>name}}</w:t></w:r></w:p></w:body></w:document>`)

	fields, err := Fields(document)
	if err != nil {
		t.Fatalf("Fields() error = %v", err)
	}
	if len(fields) != 1 || fields[0].Name != "customer_name" || fields[0].Label != "Customer Name" {
		t.Fatalf("fields = %#v", fields)
	}

	rendered, err := Render(document, map[string]string{"customer_name": "上海示例公司 & Co."})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text, err := PlainText(rendered)
	if err != nil {
		t.Fatalf("PlainText() error = %v", err)
	}
	if text != "客户：上海示例公司 & Co." {
		t.Fatalf("text = %q", text)
	}
	raw := documentXML(t, rendered)
	if !strings.Contains(raw, "上海示例公司 &amp; Co.") {
		t.Fatalf("rendered XML does not escape replacement: %s", raw)
	}
}

func TestRenderRequiresEveryTemplateValue(t *testing.T) {
	document := testDocument(t, `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>{{customer}}</w:t></w:r></w:p></w:body></w:document>`)
	if _, err := Render(document, map[string]string{}); err == nil {
		t.Fatal("Render() error = nil, want missing value error")
	}
}

func TestChinesePrototypePlaceholdersAndDefaults(t *testing.T) {
	document := testDocument(t, `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>甲方：{{客户名称}}，金额：{{合同金额}}（大写：{{金额_大写 合同金额}}），发票：{{发票类型 '专票'}}</w:t></w:r></w:p></w:body></w:document>`)

	fields, err := Fields(document)
	if err != nil {
		t.Fatalf("Fields() error = %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("fields = %#v, want 3 deduplicated input fields", fields)
	}
	if fields[0].Name != "发票类型" || fields[0].Default != "专票" || fields[1].Name != "合同金额" || fields[2].Name != "客户名称" {
		t.Fatalf("fields = %#v", fields)
	}

	rendered, err := Render(document, map[string]string{"客户名称": "示例公司", "合同金额": "10000"})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	text, err := PlainText(rendered)
	if err != nil {
		t.Fatalf("PlainText() error = %v", err)
	}
	if text != "甲方：示例公司，金额：10000（大写：壹万元整），发票：专票" {
		t.Fatalf("text = %q", text)
	}
}

func TestChineseMoneyUpper(t *testing.T) {
	tests := map[string]string{
		"0": "零元整", "10001": "壹万零壹元整", "10010.05": "壹万零壹拾元零伍分",
		"123456789.12": "壹亿贰仟叁佰肆拾伍万陆仟柒佰捌拾玖元壹角贰分",
	}
	for input, expected := range tests {
		actual, err := chineseMoneyUpper(input)
		if err != nil {
			t.Fatalf("chineseMoneyUpper(%q) error = %v", input, err)
		}
		if actual != expected {
			t.Fatalf("chineseMoneyUpper(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func testDocument(t *testing.T, documentXML string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="content-types"></Types>`,
		"word/document.xml":   documentXML,
	} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func documentXML(t *testing.T, document []byte) string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(document), int64(len(document)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	t.Fatal("word/document.xml not found")
	return ""
}
