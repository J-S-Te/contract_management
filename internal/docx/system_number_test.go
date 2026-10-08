package docx

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestReplaceSystemNumberInHeaderAndPreserveUnchangedBytes(t *testing.T) {
	original := testDocument(t, `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>正文保持不变</w:t></w:r></w:p></w:body></w:document>`)
	unchanged, err := ReplaceSystemContractNumber(original, "HT-1")
	if err != nil || !bytes.Equal(original, unchanged) {
		t.Fatalf("unchanged document rewritten: %v", err)
	}
	files, err := read(original)
	if err != nil {
		t.Fatal(err)
	}
	files["word/header1.xml"] = []byte(`<w:hdr xmlns:w="word"><w:p><w:r><w:t>【系统合同编号：审批通过后生成】</w:t></w:r></w:p></w:hdr>`)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range files {
		part, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := ReplaceSystemContractNumber(buffer.Bytes(), "HT-1")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := read(updated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(parts["word/header1.xml"], []byte("HT-1")) || !bytes.Equal(parts["word/document.xml"], files["word/document.xml"]) {
		t.Fatal("header number replacement failed or changed body")
	}
}

func TestReplaceFrozenSystemNumberAcrossRuns(t *testing.T) {
	document := testDocument(t, `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>编号：【系统合同</w:t></w:r><w:r><w:t>编号：审批通过后生成】；【系统合同编号：审批通过后生成】；客户输入HT-OLD</w:t></w:r></w:p></w:body></w:document>`)
	updated, err := ReplaceSystemContractNumber(document, "HT-2026&lt;&amp;")
	if err != nil {
		t.Fatal(err)
	}
	text, err := PlainText(updated)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(text, "HT-2026&lt;&amp;") != 2 || strings.Contains(text, SystemContractNumberPending) || !strings.Contains(text, "客户输入HT-OLD") {
		t.Fatal(text)
	}
	if _, err := ReplaceSystemContractNumber(document, ""); err == nil {
		t.Fatal("empty number accepted")
	}
	if _, err := ReplaceSystemContractNumber([]byte("broken"), "HT-1"); err == nil {
		t.Fatal("broken docx accepted")
	}
}
