package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorrectedIncomeDocuments(t *testing.T) {
	paths, err := filepath.Glob("../../收入合同模版/修正版/*.docx")
	if err != nil || len(paths) != 5 {
		t.Fatalf("want five corrected documents: %v %v", paths, err)
	}
	for i, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fields, err := Fields(source)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			for _, f := range fields {
				v := "测试" + f.Name
				switch {
				case strings.Contains(f.Name, "金额") || f.Name == "单价" || f.Name == "总价":
					v = "10000"
				case f.Name == "系统合同编号":
					v = SystemContractNumberPending
				case f.Name == "签订日期":
					v = "2026年10月8日"
				case strings.HasSuffix(f.Name, "_年"):
					v = "2026"
				case strings.HasSuffix(f.Name, "_月"):
					v = "10"
				case strings.HasSuffix(f.Name, "_日"):
					v = "8"
				case strings.Contains(f.Name, "比例"):
					v = "70"
				case strings.Contains(f.Name, "电话"):
					v = "13800000000"
				case f.Name == "数量" || f.Name == "系统数量" || f.Name == "服务期限月数":
					v = "1"
				case strings.Contains(f.Name, "邮箱"):
					v = "qa@example.invalid"
				}
				values[f.Name] = v
			}
			rendered, err := Render(source, values)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := PlainText(rendered)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "{{") || !strings.Contains(plain, "壹万元整") || strings.Contains(plain, "壹万元整元整") {
				t.Fatalf("bad substitutions: %s", plain)
			}
			if i == 1 {
				for _, fragment := range []string{"测试客户联系人", "测试我方联系人", "测试履行地点", "项目经理：测试客户项目联系人", "联系电话：13800000000"} {
					if !strings.Contains(plain, fragment) {
						t.Fatalf("missing %s", fragment)
					}
				}
				if strings.Contains(plain, "（大写）10000") {
					t.Fatal("uppercase amount remains numeric")
				}
			}
			if i == 2 && strings.Contains(plain, "（大写）10000") {
				t.Fatal("uppercase amount remains numeric")
			}
			if i == 3 {
				if _, exists := values["合同编号"]; exists {
					t.Fatal("manual contract number input retained")
				}
				approved, err := ReplaceSystemContractNumber(rendered, "HT-20261008-TEST0001")
				if err != nil {
					t.Fatal(err)
				}
				approvedText, err := PlainText(approved)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(approvedText, "合同编号：HT-20261008-TEST0001") || strings.Contains(approvedText, SystemContractNumberPending) {
					t.Fatal(approvedText)
				}
			}
			if i == 4 && (strings.Contains(plain, "2026年7月") || !strings.Contains(plain, "2026年10月8日")) {
				t.Fatal("cover date not linked")
			}
			if dir := os.Getenv("INCOME_TEMPLATE_RENDER_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), rendered, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
