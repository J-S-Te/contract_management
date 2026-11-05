package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestLocalCatalogTenantScopeVersionAudit 验证真实目录的范围、租户与版本停用边界。
// 参数：t为测试上下文，仅显式隔离CONTRACT_TEST_DSN运行。
// 返回值：无；未配置隔离测试库时跳过，不寻找业务数据库。
func TestLocalCatalogTenantScopeVersionAudit(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	t.Cleanup(func() {
		repo.db.Where("tenant_id=?", tenant).Delete(&localCounterpartyRecord{})
		repo.db.Where("tenant_id=?", tenant).Delete(&localCategoryRecord{})
	})
	ctx := context.Background()
	filter := contract.ScopeFilter{TenantID: tenant, IdentityID: "identity", AllowSelf: true}
	saved, err := repo.SaveLocalCounterparty(ctx, filter, contract.LocalCounterparty{Name: "交易对方", Enabled: true}, 0, "user")
	if err != nil || saved.Version != 1 {
		t.Fatalf("save %+v %v", saved, err)
	}
	other := filter
	other.IdentityID = "other"
	list, err := repo.ListLocalCounterparties(ctx, other, "", 20)
	if err != nil || len(list) != 0 {
		t.Fatalf("scope leak %+v %v", list, err)
	}
	other = filter
	other.TenantID = "other"
	if _, err := repo.GetLocalCounterparty(ctx, other, saved.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("tenant leak %v", err)
	}
	saved.Enabled = false
	updated, err := repo.SaveLocalCounterparty(ctx, filter, saved, 1, "user")
	if err != nil || updated.Version != 2 || updated.Enabled {
		t.Fatalf("disable %+v %v", updated, err)
	}
	if _, err := repo.SaveLocalCounterparty(ctx, filter, saved, 1, "user"); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("old version accepted %v", err)
	}
}
