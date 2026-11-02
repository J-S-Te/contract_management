package mysql

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestFormatContractNumberUsesTemplateSnapshot(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	got := formatContractNumber("CON-{YYYY}-{MM}{DD}-{ID8}", "01K000000000000000ABCDEF12", now)
	if got != "CON-2026-0801-ABCDEF12" {
		t.Fatalf("formatContractNumber() = %q", got)
	}
}

// TestFormatContractNumberUsesBeijingDate 验证日期占位符按北京日历日取值。
// 生产调用方传入的是 UTC 时刻，北京时间每天 00:00–08:00 直接用 UTC 会拿到前一天
// 的日期段（写错数据），这里用跨年、跨日边界把它固定住。
func TestFormatContractNumberUsesBeijingDate(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{
			name: "UTC 2025-12-31T16:30 是北京 2026-01-01 00:30",
			now:  time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC),
			want: "2026-01-01",
		},
		{
			name: "UTC 2025-12-31T15:30 是北京 2025-12-31 23:30",
			now:  time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC),
			want: "2025-12-31",
		},
		{
			name: "UTC 2026-01-01T03:00 是北京 2026-01-01 11:00",
			now:  time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC),
			want: "2026-01-01",
		},
		{
			name: "UTC 2026-07-31T16:00 是北京 2026-08-01 00:00",
			now:  time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC),
			want: "2026-08-01",
		},
	}
	for _, item := range cases {
		got := formatContractNumber("{YYYY}-{MM}-{DD}", "01K000000000000000ABCDEF12", item.now)
		if got != item.want {
			t.Fatalf("%s: formatContractNumber() = %q，want %q", item.name, got, item.want)
		}
	}
	// 紧凑格式（{YYYYMMDD}）与分开的占位符必须出自同一日历日。
	got := formatContractNumber("HT-{YYYYMMDD}", "01K000000000000000ABCDEF12", time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC))
	if got != "HT-20260101" {
		t.Fatalf("跨年紧凑日期段 = %q，want %q", got, "HT-20260101")
	}
}

func TestApprovedNumberUpdatesFrozenSnapshot(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>合同编号：【系统合同编号：</w:t></w:r><w:r><w:t>审批通过后生成】 原有条款</w:t></w:r></w:p></w:body></w:document>`,
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
	current := contractRecord{RenderedDocument: buffer.Bytes(), TemplateValuesJSON: []byte(`{"合同编号":"【系统合同编号：审批通过后生成】"}`)}
	updates, err := approvedNumberUpdates(current, "HT-20261008-12345678")
	if err != nil {
		t.Fatal(err)
	}
	content := updates["content"].(string)
	if !strings.Contains(content, "HT-20261008-12345678") || !strings.Contains(content, "原有条款") || strings.Contains(content, "审批通过后生成") {
		t.Fatalf("content=%q", content)
	}
	hash := sha256.Sum256(updates["rendered_document"].([]byte))
	if updates["content_hash"] != hex.EncodeToString(hash[:]) {
		t.Fatal("hash not updated")
	}
	if !strings.Contains(string(updates["template_values_json"].([]byte)), "HT-20261008-12345678") {
		t.Fatal("template values not updated")
	}
	current.TemplateValuesJSON = []byte("broken")
	if _, err := approvedNumberUpdates(current, "HT-1"); err == nil {
		t.Fatal("invalid stored metadata accepted")
	}
}
