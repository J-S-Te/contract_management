package docx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNingboAdaptedTemplates(t *testing.T) {
	paths, err := filepath.Glob("../../收入合同模版/宁波合同模板/系统适配版/*.docx")
	if err != nil || len(paths) != 2 {
		t.Fatalf("want two adapted templates, got %d: %v", len(paths), err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateExternalDocument(source); err != nil {
				t.Fatal(err)
			}
			fields, err := Fields(source)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			options := 0
			for _, f := range fields {
				v := f.Default
				if v == "" {
					v = "演示"
				}
				if strings.Contains(f.Name, "名称") {
					v = "示例单位（虚构）"
				}
				if strings.HasPrefix(f.Name, "选项_") {
					if f.Default != "□" {
						t.Fatalf("option %s must default to □", f.Name)
					}
					options++
					if options%2 == 0 {
						v = "☑"
					}
				}
				values[f.Name] = v
				if f.Name == "check" || f.Name == "系统名称" || f.Name == "单价" || f.Name == "合计" {
					t.Fatalf("shared/unsupported field: %s", f.Name)
				}
			}
			if options != 16 {
				t.Fatalf("want 16 independent options, got %d", options)
			}
			if _, ok := values["系统合同编号"]; !ok {
				t.Fatal("missing system number")
			}
			for i := 1; i <= 16; i++ {
				key := fmt.Sprintf("服务%02d_系统名称", i)
				if _, ok := values[key]; !ok {
					t.Fatalf("missing %s", key)
				}
				values[key] = fmt.Sprintf("虚构系统%02d", i)
			}
			values["系统合同编号"] = SystemContractNumberPending
			values["签订日期"] = "2026年10月23日"
			values["客户名称"] = "宁波示例技术服务验证有限公司（虚构）"
			values["付款比例_首期"], values["付款比例_尾期"] = "30%", "70%"
			values["付款金额_首期"], values["付款金额_尾期"] = "3000", "7000"
			for _, amount := range []string{"10000", "10000.25"} {
				values["合同金额"] = amount
				out, err := Render(source, values)
				if err != nil {
					t.Fatal(err)
				}
				out, err = ReplaceSystemContractNumber(out, "HT-QA-NINGBO")
				if err != nil {
					t.Fatal(err)
				}
				plain, err := PlainText(out)
				if err != nil {
					t.Fatal(err)
				}
				for _, bad := range []string{"{{", "元整元整", SystemContractNumberPending} {
					if strings.Contains(plain, bad) {
						t.Fatalf("residual %q", bad)
					}
				}
				want := "壹万元整"
				if amount == "10000.25" {
					want = "壹万元贰角伍分"
				}
				for _, fragment := range []string{want, "HT-QA-NINGBO", "□", "☑", "虚构系统01", "虚构系统16"} {
					if !strings.Contains(plain, fragment) {
						t.Fatalf("missing %q", fragment)
					}
				}
				for i := 1; i <= 16; i++ {
					name := fmt.Sprintf("虚构系统%02d", i)
					if strings.Count(plain, name) != 1 {
						t.Fatalf("service row value %s must occur exactly once", name)
					}
				}
				if strings.Count(plain, values["客户名称"]) < 2 {
					t.Fatal("shared customer name must appear consistently in cover and signing block")
				}
				if amount == "10000" {
					if dir := os.Getenv("NINGBO_TEMPLATE_RENDER_DIR"); dir != "" {
						if err := os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, filepath.Base(path)), out, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		})
	}
}
