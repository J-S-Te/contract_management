package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

// conflictIdentityError 返回一个「已导入」冲突错误，模拟 con_import_identity 已有行。
// 参数：contractID 为既有合同（空串表示身份已占用但未建单）。
// 返回值：带定位信息的冲突错误。
func conflictIdentityError(contractID string) error {
	return &ImportIdentityConflictError{ContractID: contractID, ItemID: "item-original",
		Kind: ImportConflictKindAlreadyImported}
}

// TestImportIdentityConflictIssueIsActionable 验证来源身份冲突落到逐项 Issue 后
// 带出既有合同/导入项、冲突类别与可操作文案，且预检不再把它当成可重试依赖故障。
// 参数：t 为测试上下文。
// 返回值：错误码、可重试标记、定位字段或文案不符时失败。
func TestImportIdentityConflictIssueIsActionable(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantContract string
		wantContains string
		wantNot      string
	}{
		{name: "already-imported-with-contract", err: conflictIdentityError("contract-imported"),
			wantContract: "contract-imported", wantContains: "contract-imported",
			wantNot: "未关联合同"},
		{name: "already-imported-without-contract", err: conflictIdentityError(""),
			wantContains: "未关联合同"},
		{name: "baseline-drift", err: &ImportIdentityConflictError{Kind: ImportConflictKindBaselineDrift},
			wantContains: "重新预检"},
		{name: "ledger-mismatch", err: &ImportIdentityConflictError{ItemID: "item-original", Kind: ImportConflictKindLedgerMismatch},
			wantContains: "开立台账"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 错误分类与 errors.Is 语义必须保持不变（HTTP 映射依赖它）。
			if !errors.Is(testCase.err, ErrImportIdentityConflict) {
				t.Fatal("Unwrap 丢失了 ErrImportIdentityConflict 语义")
			}
			if code := importErrorCode(testCase.err); code != "IMPORT_SOURCE_ID_CONFLICT" {
				t.Fatalf("错误分类=%s want IMPORT_SOURCE_ID_CONFLICT", code)
			}
			for _, mapped := range []struct {
				field string
				issue contract.ImportIssue
			}{
				{field: "commit", issue: importItemIssue(testCase.err)},
				{field: "metadata", issue: previewImportItemIssue(testCase.err)},
			} {
				issue := mapped.issue
				if issue.Field != mapped.field || issue.Code != "IMPORT_SOURCE_ID_CONFLICT" || issue.Retryable {
					t.Fatalf("%s 链路 Issue=%+v want field=%s code=IMPORT_SOURCE_ID_CONFLICT retryable=false",
						mapped.field, issue, mapped.field)
				}
				if issue.Code == "IMPORT_DEPENDENCY_UNAVAILABLE" {
					t.Fatal("身份冲突被误判为可重试依赖故障")
				}
				// 基线漂移没有可指的既有合同/导入项；已导入与台账不一致才带定位。
				wantItem := "item-original"
				wantKind := ImportConflictKindAlreadyImported
				switch testCase.err.(*ImportIdentityConflictError).Kind {
				case ImportConflictKindBaselineDrift:
					wantKind, wantItem = ImportConflictKindBaselineDrift, ""
				case ImportConflictKindLedgerMismatch:
					wantKind, wantItem = ImportConflictKindLedgerMismatch, "item-original"
				}
				if issue.ContractID != testCase.wantContract || issue.ItemID != wantItem {
					t.Fatalf("Issue 定位=%+v want contract_id=%q item_id=%q", issue, testCase.wantContract, wantItem)
				}
				if issue.ConflictKind != wantKind {
					t.Fatalf("conflict_kind=%s want %s", issue.ConflictKind, wantKind)
				}
				if !strings.Contains(issue.Message, testCase.wantContains) {
					t.Fatalf("message=%q 缺少 %q", issue.Message, testCase.wantContains)
				}
				if testCase.wantNot != "" && strings.Contains(issue.Message, testCase.wantNot) {
					t.Fatalf("message=%q 不应包含 %q", issue.Message, testCase.wantNot)
				}
			}
			// 原错误类型仍可被 As 取到（调度器与预检共用同一投影）。
			var conflict *ImportIdentityConflictError
			if !errors.As(testCase.err, &conflict) || conflict.Kind == "" {
				t.Fatalf("errors.As 未取回冲突细节: %#v", conflict)
			}
		})
	}
}

// TestImportItemTerminalClassifiesRetryableBlocked 验证终态只收「身份冲突/纠错/复核」，
// 围栏/权限/校验/依赖类 BLOCKED 仍可由 resume 恢复。
// 参数：t 为测试上下文。
// 返回值：终态/可推进判定不符时失败。
func TestImportItemTerminalClassifiesRetryableBlocked(t *testing.T) {
	cases := []struct {
		name string
		item contract.ImportItemState
		want bool
	}{
		{name: "correction-required", item: contract.ImportItemState{State: "CORRECTION_REQUIRED"}, want: true},
		{name: "needs-review", item: contract.ImportItemState{State: "NEEDS_REVIEW"}, want: true},
		{name: "blocked-identity-conflict", want: true, item: contract.ImportItemState{State: "BLOCKED",
			Issues: []contract.ImportIssue{{Code: "IMPORT_SOURCE_ID_CONFLICT"}}}},
		{name: "blocked-revision-conflict", want: false, item: contract.ImportItemState{State: "BLOCKED",
			Issues: []contract.ImportIssue{{Code: "IMPORT_REVISION_CONFLICT"}}}},
		{name: "blocked-retryable", item: contract.ImportItemState{State: "BLOCKED",
			Issues: []contract.ImportIssue{{Code: "IMPORT_DEPENDENCY_UNAVAILABLE", Retryable: true}}}},
		{name: "blocked-without-issue", item: contract.ImportItemState{State: "BLOCKED"}},
		{name: "ready", item: contract.ImportItemState{State: "READY"}},
		{name: "needs-provision", item: contract.ImportItemState{State: contract.ImportItemStateNeedsProvision}},
	}
	for _, testCase := range cases {
		if got := importItemTerminal(testCase.item); got != testCase.want {
			t.Fatalf("%s: terminal=%v want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestImportDispatchSkipsTerminalBlockedItem 验证一个不可重试的 BLOCKED 行不会卡死整批：
// 调度器跳过它执行后续行，剩余项全部为终态时批次转 PARTIAL 而不是重排 QUEUED 空转。
// 参数：t 为测试上下文。
// 返回值：重复处理首项、邻行未执行或批次状态不符时失败。
func TestImportDispatchSkipsTerminalBlockedItem(t *testing.T) {
	ctx := context.Background()
	pdf := testfixture.PDF("identity conflict batch")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	blocked := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	healthy := provisionHistoryItem("two", "old-2", "source-two", "历史客户", "张三", 654321)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{blocked, healthy},
		provisionTestSource("one", pdf), provisionTestSource("two", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)
	// 模拟第一项在提交阶段遇到不可重试的来源身份冲突。
	terminalIssue := contract.ImportIssue{Field: "commit", Code: "IMPORT_SOURCE_ID_CONFLICT",
		ContractID: "contract-existing", ItemID: "item-original", ConflictKind: ImportConflictKindAlreadyImported,
		Message: "该来源单号此前已导入为合同 contract-existing"}
	repo.batch.Items[0].State = "BLOCKED"
	repo.batch.Items[0].Issues = []contract.ImportIssue{terminalIssue}
	service.ImportActorResolver = func(context.Context, []byte, string, string) (Principal, error) {
		return actor, nil
	}

	// 第一次调度：必须跳过终态首项，推进并完成第二项。
	if err := service.DispatchImportOnce(ctx); err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if repo.batch.Items[1].State != "COMPLETE" {
		t.Fatalf("第二项未执行: state=%s issues=%+v", repo.batch.Items[1].State, repo.batch.Items[1].Issues)
	}
	if repo.batch.Items[0].State != "BLOCKED" || !reflect.DeepEqual(repo.batch.Items[0].Issues, []contract.ImportIssue{terminalIssue}) {
		t.Fatalf("首项被重新处理: %+v", repo.batch.Items[0])
	}
	if repo.commits != 1 {
		t.Fatalf("建单次数=%d want 1", repo.commits)
	}
	if repo.batch.State != "QUEUED" {
		t.Fatalf("批次状态=%s want QUEUED", repo.batch.State)
	}

	// 第二次调度：只剩终态项，批次必须转 PARTIAL，不能重排 QUEUED 或重复建单。
	if err := service.DispatchImportOnce(ctx); err != nil {
		t.Fatalf("second dispatch error: %v", err)
	}
	if repo.batch.State != "PARTIAL" {
		t.Fatalf("批次状态=%s want PARTIAL", repo.batch.State)
	}
	if repo.commits != 1 || calls.createCustomer != 1 {
		t.Fatalf("重复执行: commits=%d createCustomer=%d", repo.commits, calls.createCustomer)
	}
	if repo.batch.Items[0].State != "BLOCKED" || !reflect.DeepEqual(repo.batch.Items[0].Issues, []contract.ImportIssue{terminalIssue}) {
		t.Fatalf("首项终态被改写: %+v", repo.batch.Items[0])
	}
}

// TestImportResumeKeepsTerminalRows 验证 resume 只重置可重试项：
// 不可重试的终态 BLOCKED 行保留 State/Issues，避免「恢复 → 再失败」循环。
// 参数：t 为测试上下文。
// 返回值：终态行被清空或可重试行未被重置时失败。
func TestImportResumeKeepsTerminalRows(t *testing.T) {
	actor := provisionImportActor()
	terminalIssue := contract.ImportIssue{Field: "commit", Code: "IMPORT_SOURCE_ID_CONFLICT",
		Retryable: false, ContractID: "contract-existing", ItemID: "item-original",
		ConflictKind: ImportConflictKindAlreadyImported, Message: "该来源单号此前已导入为合同 contract-existing"}
	retryableIssue := contract.ImportIssue{Field: "commit", Code: "IMPORT_DEPENDENCY_UNAVAILABLE", Retryable: true}
	revisionIssue := contract.ImportIssue{Field: "commit", Code: "IMPORT_REVISION_CONFLICT", Retryable: false}
	repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: actor.TenantID, ActorID: actor.UserID,
		Version: 2, Revision: 1, CommittedRevision: 1, OperationID: "op", State: "PARTIAL",
		Items: []contract.ImportItemState{
			{ID: "item-terminal", State: "BLOCKED", Issues: []contract.ImportIssue{terminalIssue}},
			{ID: "item-retryable", State: "BLOCKED", Issues: []contract.ImportIssue{retryableIssue}},
			{ID: "item-revision", State: "BLOCKED", Issues: []contract.ImportIssue{revisionIssue}},
			{ID: "item-created", State: "CREATED", ContractID: "contract-1"},
		}}}
	service := &Service{Repo: repo}
	if err := service.ControlImport(context.Background(), actor, "batch", 2, true, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if repo.batch.State != "QUEUED" {
		t.Fatalf("批次状态=%s want QUEUED", repo.batch.State)
	}
	terminal := repo.batch.Items[0]
	if terminal.State != "BLOCKED" || !reflect.DeepEqual(terminal.Issues, []contract.ImportIssue{terminalIssue}) {
		t.Fatalf("终态项被重置: %+v", terminal)
	}
	retryable := repo.batch.Items[1]
	if retryable.State != "READY" || retryable.Issues != nil {
		t.Fatalf("可重试项未重置: %+v", retryable)
	}
	revision := repo.batch.Items[2]
	if revision.State != "READY" || revision.Issues != nil {
		t.Fatalf("围栏冲突项应可重试: %+v", revision)
	}
	if created := repo.batch.Items[3]; created.State != "CREATED" || created.ContractID != "contract-1" {
		t.Fatalf("已建单行被改写: %+v", created)
	}
}
