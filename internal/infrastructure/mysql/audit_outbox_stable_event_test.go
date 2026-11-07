package mysql

import (
	"strings"
	"testing"
)

// TestStableEventIDIsDeterministicAndScoped 验证自动路径事件 ID 的派生契约：
// 同一组业务键恒定返回同一 26 字符 id（可重放对齐），任一维度变化都会得到不同 id，
// 且编码与 ULID 的 Crockford base32 逐字符一致（跨实现向量）。
//
// 为什么必须锁死向量：event_id 是源库与审计平台去重的唯一依据。编码或分帧一旦变化，
// 重放就会写出第二条事件，历史对账也会错位，因此这里用外部实现算出的固定向量断言。
//
// 参数：t 为测试上下文（纯函数，不连库）。
// 返回值：长度、稳定性、区分度或固定向量不符时失败。
func TestStableEventIDIsDeterministicAndScoped(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "auto-approve-v1", parts: []string{"tenant-1", "batch-1", "item-1", "IMPORT_AUTO_APPROVED", "1"}, want: "1MTVENMY5R52XJ8WG9SMHADR8P"},
		{name: "auto-approve-v2", parts: []string{"tenant-1", "batch-1", "item-1", "IMPORT_AUTO_APPROVED", "2"}, want: "1GFMSHSK3FN0MAQ07A065W7D7P"},
		{name: "confirmed-v1", parts: []string{"tenant-1", "batch-1", "item-1", "IMPORT_CONFIRMED", "1"}, want: "52QFKSBDTFZ6Z836X64KTY8YYQ"},
		{name: "other-item", parts: []string{"tenant-1", "batch-1", "item-2", "IMPORT_AUTO_APPROVED", "1"}, want: "0P9Y2QD3TG8JFZHMV03Y1W8H5A"},
		{name: "other-tenant", parts: []string{"tenant-2", "batch-1", "item-1", "IMPORT_AUTO_APPROVED", "1"}, want: "31METV2VAS85H3GYQ9XZE47CGY"},
		{name: "other-batch", parts: []string{"tenant-1", "batch-2", "item-1", "IMPORT_AUTO_APPROVED", "1"}, want: "653EBBA3QQYCTDWMY614BAR3XR"},
		{name: "max-version", parts: []string{"01J00000000000000000000000", "01J00000000000000000000001", "01J00000000000000000000002", "IMPORT_AUTO_APPROVED", "18446744073709551615"}, want: "46ETBKQJ6P4NSJ1XV9TD05B8F7"},
		{name: "zero-version", parts: []string{"T", "B", "I", "IMPORT_CONFIRMED", "0"}, want: "4ENJ51P32Z6JF131F8XSVRJ689"},
		{name: "empty-segments", parts: []string{"", "", "", "IMPORT_AUTO_APPROVED", "1"}, want: "4KMMNM5P9N45D5H2XTD7DCGZZ8"},
		{name: "short-segments", parts: []string{"a", "b", "c", "IMPORT_CONFIRMED", "7"}, want: "5D4FRWYMT6F9VB7DV0QX364M5Z"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := stableEventID(testCase.parts...)
			if got != testCase.want {
				t.Fatalf("stableEventID(%q)=%q, want %q", testCase.parts, got, testCase.want)
			}
			// CHAR(26)：长度必须与 newID() 一致，否则写入会被截断或报错。
			if len(got) != 26 {
				t.Fatalf("length=%d, want 26", len(got))
			}
			// 重放稳定性：同一业务键必须永远得到同一 id。
			if again := stableEventID(testCase.parts...); again != got {
				t.Fatalf("同一业务键两次派生不同 id: %q != %q", got, again)
			}
		})
	}

	// 区分度：自动审批与自动确认是两条独立事实，即使定位同一个单项也不能撞 id。
	auto := stableEventID("tenant-1", "batch-1", "item-1", "IMPORT_AUTO_APPROVED", "1")
	confirmed := stableEventID("tenant-1", "batch-1", "item-1", "IMPORT_CONFIRMED", "1")
	if auto == confirmed {
		t.Fatalf("不同动作派生出同一 event_id: %q", auto)
	}
	// 编码域：只含 Crockford base32 大写字母（不含 I/L/O/U），可被平台侧 ULID 解析接受。
	for _, id := range []string{auto, confirmed} {
		if id != strings.ToUpper(id) || strings.ContainsAny(id, "ILOU") {
			t.Fatalf("event_id=%q 不是合法 Crockford base32", id)
		}
	}
}
