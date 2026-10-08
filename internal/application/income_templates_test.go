package application

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/docx"
)

func TestIncomeTemplatesRealDocuments(t *testing.T) {
	paths, err := filepath.Glob("../../收入合同模版/[1-5]、*.docx")
	if err != nil || len(paths) != 5 {
		t.Fatalf("income templates: paths=%v error=%v; want five real documents", paths, err)
	}
	// The two 金额_大写 helpers reuse 合同金额 rather than creating another input.
	expectedFields := []int{21, 25, 21, 44, 24}
	actor := Principal{TenantID: "income-tenant", UserID: "template-manager", Permissions: map[string]bool{"contract.template.manage": true, "contract.create": true}}
	for index, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fields, err := docx.Fields(source)
			if err != nil {
				t.Fatal(err)
			}
			if len(fields) != expectedFields[index] {
				t.Fatalf("field count=%d want=%d: %+v", len(fields), expectedFields[index], fields)
			}
			values := make(map[string]string, len(fields))
			for _, field := range fields {
				if field.Label == "" || strings.Contains(field.Name, "金额_大写 ") {
					t.Fatalf("invalid field: %+v", field)
				}
				values[field.Name] = "回归<&>\"“" + field.Name
			}
			if _, exists := values["合同金额"]; !exists {
				t.Fatal("missing 合同金额")
			}
			values["合同金额"] = "123456.78"
			repository := &memoryTemplateRepository{}
			service := &Service{Templates: repository}
			created, err := service.CreateTemplate(context.Background(), actor, strings.TrimSuffix(filepath.Base(path), ".docx"), filepath.Base(path), source)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(repository.items[created.ID].Content, source) {
				t.Fatal("source document changed during import")
			}
			rendered, normalized, err := service.renderTemplate(context.Background(), actor, created.ID, values)
			if err != nil {
				t.Fatal(err)
			}
			if len(normalized) != len(fields) {
				t.Fatalf("normalized fields=%d want=%d", len(normalized), len(fields))
			}
			incomeAssertXML(t, rendered)
			text, err := docx.PlainText(rendered)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, "{{") || !strings.Contains(text, values["客户名称"]) {
				t.Fatalf("placeholders were not replaced: %s", text)
			}
			preview, err := service.PreviewTemplate(context.Background(), actor, created.ID, values)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(preview, html.EscapeString(values["客户名称"])) || strings.Contains(preview, "{{") {
				t.Fatalf("unsafe or unresolved preview: %s", preview)
			}
			if index == 0 || index == 4 {
				for label, result := range map[string]string{"text": text, "preview": preview} {
					if !strings.Contains(result, "壹拾贰万叁仟肆佰伍拾陆元柒角捌分") {
						t.Fatalf("%s missing converted amount: %s", label, result)
					}
				}
				invalid := make(map[string]string, len(values))
				for key, value := range values {
					invalid[key] = value
				}
				invalid["合同金额"] = "非法金额"
				if _, err := service.PreviewTemplate(context.Background(), actor, created.ID, invalid); !errors.Is(err, ErrValidation) {
					t.Fatalf("invalid amount error=%v want ErrValidation", err)
				}
			}
			delete(values, "客户名称")
			if _, err := service.PreviewTemplate(context.Background(), actor, created.ID, values); !errors.Is(err, ErrValidation) {
				t.Fatalf("missing field error=%v want ErrValidation", err)
			}
		})
	}
}

func incomeAssertXML(t *testing.T, document []byte) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(document), int64(len(document)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		if !strings.HasSuffix(file.Name, ".xml") && !strings.HasSuffix(file.Name, ".rels") {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		decoder := xml.NewDecoder(stream)
		for {
			_, err = decoder.Token()
			if err != nil {
				break
			}
		}
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != io.EOF {
			t.Fatalf("%s is invalid XML: %v", file.Name, err)
		}
	}
}
