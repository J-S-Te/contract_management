package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

type importNamedOwnerStub struct {
	err   error
	calls int
}

// TestImportOwnerNameScope 验证姓名解析不扩大委派权限，非法范围先拒绝再查询目录。
// 参数：t为负责人权限边界测试上下文。
// 返回值：无；组织/项目范围被当成SELF、账号冲突或无权限查询目录时失败。
func TestImportOwnerNameScope(t *testing.T) {
	for _, tc := range []struct {
		name           string
		scope          contract.ScopeFilter
		id             string
		allowed, query bool
	}{
		{"全范围", contract.ScopeFilter{AllowAll: true}, "", true, true},
		{"本人", contract.ScopeFilter{AllowSelf: true}, "", true, true},
		{"仅组织", contract.ScopeFilter{OrganizationIDs: []string{"org"}}, "", false, false},
		{"仅项目", contract.ScopeFilter{ProjectIDs: []string{"project"}}, "", false, false},
		{"姓名账号冲突", contract.ScopeFilter{AllowAll: true}, "other-user", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := &importNamedOwnerStub{}
			svc := &Service{ImportOwners: directory}
			actor := Principal{TenantID: "tenant", UserID: "actor", Permissions: map[string]bool{"contract.import.commit": true},
				PermissionScopes: map[string]contract.ScopeFilter{"contract.import.commit": tc.scope}}
			_, err := svc.importHistoryOwner(context.Background(), actor, contract.ImportItem{
				HistoryEvidence: &contract.ImportHistoryEvidence{TargetOwnerUserID: tc.id, TargetOwnerName: "当前责任人"}})
			if tc.allowed && err != nil || !tc.allowed && !errors.Is(err, ErrForbidden) {
				t.Fatalf("error=%v", err)
			}
			if (directory.calls > 0) != tc.query {
				t.Fatal("无可用委派范围不得查询目录")
			}
		})
	}
}

// ResolveOwnerByName 提供唯一用户或注入目录错误，不以销售身份代替当前负责人。
// 参数：ctx、tenant和name为目录查询身份及姓名。
// 返回值：有效用户或测试指定分类错误。
func (s *importNamedOwnerStub) ResolveOwnerByName(ctx context.Context, tenant, name string) (UserReference, error) {
	s.calls++
	return UserReference{UserID: "actor", DisplayName: "当前责任人"}, s.err
}

// ResolveOwner 复核已解析用户，保留当前负责人原有ID校验边界。
// 参数：ctx、tenant和id为用户权威回读条件。
// 返回值：当前有效用户。
func (s *importNamedOwnerStub) ResolveOwner(ctx context.Context, tenant, id string) (UserReference, error) {
	return UserReference{UserID: id, DisplayName: "当前责任人"}, nil
}

// TestImportExcelOwnerNamePreview 验证Excel姓名在预检持久化前解析，失败逐行阻断且不创建客户。
// 参数：t为真实预检编排的测试上下文。
// 返回值：无；姓名丢失、错绑用户或失败继续建档时失败。
func TestImportExcelOwnerNamePreview(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"唯一", nil, ""}, {"重名", ErrProvisionOwnerAmbiguous, "IMPORT_CURRENT_OWNER_AMBIGUOUS"},
		{"未找到", ErrProvisionOwnerNotFound, "IMPORT_CURRENT_OWNER_NOT_FOUND"},
		{"不可用", ErrProvisionOwnerDirectoryUnavailable, "IMPORT_CURRENT_OWNER_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pdf := testfixture.PDF("historical owner contract")
			item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			item.HistoryEvidence.TargetOwnerUserID = ""
			item.HistoryEvidence.TargetOwnerName = "当前责任人"
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
			calls, customer, opportunity, sales := newProvisionStubs()
			svc := provisionTestService(repo, pdf, customer, opportunity, sales)
			directory := &importNamedOwnerStub{err: tc.err}
			svc.ImportOwners = directory
			result, err := svc.PreviewImport(context.Background(), provisionImportActor(), "batch", 1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.err == nil {
				if result.State != "PREVIEWED" || result.Items[0].Item.HistoryEvidence.TargetOwnerUserID != "actor" {
					t.Fatalf("姓名未自动带入真实账号: %+v", result.Items[0])
				}
			} else if result.State != "BLOCKED" || result.Items[0].Issues[0].Code != tc.code || calls.lookup != 0 {
				t.Fatalf("失败应逐行阻断且不查询CRM: %+v", result.Items[0])
			}
			if calls.createCustomer != 0 {
				t.Fatal("预检不得创建客户")
			}
		})
	}
}
