package contract

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// provisionKey 返回符合主键列约束的测试幂等键。
// 参数：无。
// 返回值：26 位租户 + 合法来源系统/外部记录 ID。
func provisionKey() ImportCRMProvisionKey {
	return ImportCRMProvisionKey{
		TenantID:         "01J00000000000000000000000",
		SourceSystem:     "crm",
		ExternalRecordID: "history-0001",
	}
}

// TestImportCRMProvisionTransitionMatrix 验证状态机只允许严格下一步与进入 BLOCKED。
// 参数：t 为测试上下文。
// 返回值：无；出现跳步、自转或终态回退时失败。
func TestImportCRMProvisionTransitionMatrix(t *testing.T) {
	order := []string{
		ImportCRMProvisionPending, ImportCRMProvisionCustomerReady, ImportCRMProvisionContractReady,
		ImportCRMProvisionOpportunityReady, ImportCRMProvisionCompleted,
	}
	for fromIndex, from := range order {
		for toIndex, to := range order {
			want := toIndex == fromIndex+1
			if got := ImportCRMProvisionTransitionAllowed(from, to); got != want {
				t.Fatalf("%s -> %s = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, from := range order[:len(order)-1] {
		if !ImportCRMProvisionTransitionAllowed(from, ImportCRMProvisionBlocked) {
			t.Fatalf("%s -> BLOCKED must be allowed", from)
		}
	}
	for _, pair := range [][2]string{
		{ImportCRMProvisionCompleted, ImportCRMProvisionBlocked},
		{ImportCRMProvisionBlocked, ImportCRMProvisionPending},
		{ImportCRMProvisionBlocked, ImportCRMProvisionBlocked},
		{ImportCRMProvisionCompleted, ImportCRMProvisionCompleted},
		{"", ImportCRMProvisionPending},
		{ImportCRMProvisionPending, ""},
		{ImportCRMProvisionPending, "UNKNOWN"},
	} {
		if ImportCRMProvisionTransitionAllowed(pair[0], pair[1]) {
			t.Fatalf("%s -> %s must be rejected", pair[0], pair[1])
		}
	}
	if ImportCRMProvisionNextState(ImportCRMProvisionOpportunityReady) != ImportCRMProvisionCompleted ||
		ImportCRMProvisionNextState(ImportCRMProvisionCompleted) != "" ||
		!ImportCRMProvisionTerminal(ImportCRMProvisionBlocked) ||
		ImportCRMProvisionTerminal(ImportCRMProvisionContractReady) {
		t.Fatal("next/terminal helpers disagree with the state machine")
	}
}

// TestImportCRMProvisionAdvanceRequiresSequentialStateAndAttempt 验证推进的纯函数语义：
// 顺序、CAS attempt、必要 CRM 标识与失败痕迹清理。
// 参数：t 为测试上下文。
// 返回值：无；任一不变量不符时失败。
func TestImportCRMProvisionAdvanceRequiresSequentialStateAndAttempt(t *testing.T) {
	start := ImportCRMProvision{ImportCRMProvisionKey: provisionKey(), State: ImportCRMProvisionPending, Attempt: 0}
	advance := ImportCRMProvisionAdvance{From: ImportCRMProvisionPending, To: ImportCRMProvisionCustomerReady, ExpectedAttempt: 0, CustomerID: 11, CustomerCreated: true}
	next, err := start.Advance(advance)
	if err != nil || next.State != ImportCRMProvisionCustomerReady || next.Attempt != 1 || next.CustomerID != 11 || !next.CustomerCreated {
		t.Fatalf("advance=%+v err=%v", next, err)
	}
	// 同一前置条件重复提交不得再产生推进（state/attempt 已前进）。
	if _, err := next.Advance(advance); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("replaying an applied advance error=%v", err)
	}
	// attempt 过期必须报专有错误，便于上层区分并发冲突与非法跳步。
	stale := ImportCRMProvisionAdvance{From: ImportCRMProvisionCustomerReady, To: ImportCRMProvisionContractReady, ExpectedAttempt: 0}
	if _, err := next.Advance(stale); !errors.Is(err, ErrImportCRMProvisionAttempt) {
		t.Fatalf("stale attempt error=%v", err)
	}
	// 跳步与终态回退必须拒绝。
	skip := ImportCRMProvisionAdvance{From: ImportCRMProvisionPending, To: ImportCRMProvisionContractReady, ExpectedAttempt: 0}
	if _, err := start.Advance(skip); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("skip step error=%v", err)
	}
	if _, err := next.Advance(ImportCRMProvisionAdvance{From: ImportCRMProvisionCustomerReady, To: ImportCRMProvisionCustomerReady, ExpectedAttempt: 1}); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("self transition error=%v", err)
	}
	// CUSTOMER_READY / OPPORTUNITY_READY 必须携带真实 CRM 标识。
	if _, err := start.Advance(ImportCRMProvisionAdvance{From: ImportCRMProvisionPending, To: ImportCRMProvisionCustomerReady, ExpectedAttempt: 0}); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("customer ready without id error=%v", err)
	}
	contractReady := ImportCRMProvision{ImportCRMProvisionKey: provisionKey(), State: ImportCRMProvisionContractReady, Attempt: 2}
	if _, err := contractReady.Advance(ImportCRMProvisionAdvance{From: ImportCRMProvisionContractReady, To: ImportCRMProvisionOpportunityReady, ExpectedAttempt: 2}); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("opportunity ready without id error=%v", err)
	}
	// 失败痕迹在推进成功后清空。
	dirty := ImportCRMProvision{ImportCRMProvisionKey: provisionKey(), State: ImportCRMProvisionContractReady, Attempt: 3,
		LastErrorCode: "IMPORT_PROVISION_CRM_REJECTED", LastErrorMessage: "旧失败"}
	clean, err := dirty.Advance(ImportCRMProvisionAdvance{From: ImportCRMProvisionContractReady, To: ImportCRMProvisionOpportunityReady, ExpectedAttempt: 3, OpportunityID: 92, OpportunityCreated: true})
	if err != nil || clean.OpportunityID != 92 || !clean.OpportunityCreated || clean.LastErrorCode != "" || clean.LastErrorMessage != "" {
		t.Fatalf("clean advance=%+v err=%v", clean, err)
	}
}

// TestImportCRMProvisionKeyAndValueValidation 验证幂等键与落账列宽约束。
// 参数：t 为测试上下文。
// 返回值：无；越界值被接受时失败。
func TestImportCRMProvisionKeyAndValueValidation(t *testing.T) {
	if err := provisionKey().Validate(); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, broken := range []ImportCRMProvisionKey{
		{TenantID: "short", SourceSystem: "crm", ExternalRecordID: "r1"},
		{TenantID: provisionKey().TenantID, SourceSystem: "", ExternalRecordID: "r1"},
		{TenantID: provisionKey().TenantID, SourceSystem: "crm system", ExternalRecordID: "r1"},
		{TenantID: provisionKey().TenantID, SourceSystem: "crm", ExternalRecordID: strings.Repeat("x", 129)},
	} {
		if err := broken.Validate(); !errors.Is(err, ErrImportCRMProvisionKey) {
			t.Fatalf("broken key %+v error=%v", broken, err)
		}
	}
	value := ImportCRMProvision{ImportCRMProvisionKey: provisionKey(), State: ImportCRMProvisionPending}
	if err := value.Validate(); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
	value.State = "UNKNOWN"
	if err := value.Validate(); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("unknown state error=%v", err)
	}
	value = ImportCRMProvision{ImportCRMProvisionKey: provisionKey(), State: ImportCRMProvisionPending, Attempt: -1}
	if err := value.Validate(); !errors.Is(err, ErrImportCRMProvisionState) {
		t.Fatalf("negative attempt error=%v", err)
	}
}

// TestTruncateImportCRMProvisionMessageKeepsUTF8 验证错误消息按字符边界截断到列宽。
// 参数：t 为测试上下文。
// 返回值：无；截断后超宽或出现半个字符时失败。
func TestTruncateImportCRMProvisionMessageKeepsUTF8(t *testing.T) {
	short := "CRM 拒绝"
	if got := TruncateImportCRMProvisionMessage(short); got != short {
		t.Fatalf("short message changed: %q", got)
	}
	long := strings.Repeat("合", 200) // 600 字节
	got := TruncateImportCRMProvisionMessage(long)
	if len(got) > ImportCRMProvisionMaxErrorMessageBytes || !utf8.ValidString(got) || got == "" {
		t.Fatalf("truncated message invalid: bytes=%d valid=%v", len(got), utf8.ValidString(got))
	}
	// 512 字节边界落在第三个字节处，因此应保留 170 个汉字（510 字节）。
	if len(got) != 510 {
		t.Fatalf("unexpected truncation length %d", len(got))
	}
	if err := ValidateImportCRMProvisionErrorCode(""); !errors.Is(err, ErrImportCRMProvisionError) {
		t.Fatalf("empty code error=%v", err)
	}
	if err := ValidateImportCRMProvisionErrorCode(strings.Repeat("X", 65)); !errors.Is(err, ErrImportCRMProvisionError) {
		t.Fatalf("oversized code error=%v", err)
	}
	if err := ValidateImportCRMProvisionErrorCode("IMPORT_PROVISION_OWNER_UNRESOLVED"); err != nil {
		t.Fatalf("valid code rejected: %v", err)
	}
}
