package contract

import "testing"

// TestImportOperationalOrigin 验证旧数据兼容与未知历史策略失败关闭。
// 参数：t为测试上下文。
// 返回值：无；历史行或未知行进入普通派生路径时失败。
func TestImportOperationalOrigin(t *testing.T) {
	if !OperationalOrigin(Contract{}) || !OperationalOrigin(Contract{OriginKind: "NORMAL", ImportMode: ImportNormal}) {
		t.Fatal("普通存量不兼容")
	}
	for _, c := range []Contract{{OriginKind: "IMPORTED", ImportMode: ImportArchiveOnly}, {OriginKind: "IMPORTED", ImportMode: ImportContinueLive}, {OriginKind: "UNKNOWN"}, {OriginKind: "NORMAL", ImportMode: "UNKNOWN"}, {ImportPolicy: &ImportPolicy{Version: 1}}} {
		if OperationalOrigin(c) {
			t.Fatalf("异常来源进入普通路径: %s/%s", c.OriginKind, c.ImportMode)
		}
	}
}
