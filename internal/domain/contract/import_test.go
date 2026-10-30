package contract

import "testing"

// TestImportManifestIdentity 验证来源身份及首版模式边界。
// 参数：t为测试上下文。
// 返回值：无；重复键或非规范身份未被拒绝时失败。
func TestImportManifestIdentity(t *testing.T) {
	m := ImportManifest{SchemaVersion: 1, SourceSystem: "legacy", Items: []ImportItem{{Key: "one", ExternalRecordID: "A", Mode: ImportNormal, SourceFileRef: "file"}}}
	if err := m.Validate(100); err != nil {
		t.Fatal(err)
	}
	m.Items = append(m.Items, m.Items[0])
	if m.Validate(100) == nil {
		t.Fatal("重复来源身份被接受")
	}
	m.Items = m.Items[:1]
	m.Items[0].Mode = ImportArchiveOnly
	if m.Validate(100) == nil {
		t.Fatal("历史模式缺少策略被接受")
	}
}
