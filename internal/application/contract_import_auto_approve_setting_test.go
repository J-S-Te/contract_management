package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// autoApproveSettingRepository 是运行开关读写用例的测试仓储。
// 它只实现设置接口，其余能力由 recordingRepository 兜底，避免把用例绑到真实表。
type autoApproveSettingRepository struct {
	recordingRepository
	setting  contract.ImportAutoApproveSetting
	getErr   error
	setErr   error
	getCalls int
	setCalls int
	tenantID string
	enabled  bool
	expected uint64
	actorID  string
}

// GetImportAutoApproveSetting 记录读取次数并返回预置设置或错误。
// 参数：ctx 为上下文，tenantID 为租户。
// 返回值：预置设置；注入错误时返回错误。
func (r *autoApproveSettingRepository) GetImportAutoApproveSetting(_ context.Context, tenantID string) (contract.ImportAutoApproveSetting, error) {
	r.getCalls++
	r.tenantID = tenantID
	if r.getErr != nil {
		return contract.ImportAutoApproveSetting{}, r.getErr
	}
	return r.setting, nil
}

// SetImportAutoApproveSetting 记录写入参数并返回预置结果。
// 参数：ctx 为上下文，tenantID/enabled/expectedVersion/actorID 为写入契约。
// 返回值：预置错误或推进版本后的设置。
func (r *autoApproveSettingRepository) SetImportAutoApproveSetting(_ context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error) {
	r.setCalls++
	r.tenantID, r.enabled, r.expected, r.actorID = tenantID, enabled, expectedVersion, actorID
	if r.setErr != nil {
		return contract.ImportAutoApproveSetting{}, r.setErr
	}
	return contract.ImportAutoApproveSetting{
		TenantID: tenantID, Enabled: enabled, Version: expectedVersion + 1,
		UpdatedBy: actorID, UpdatedAt: time.Now().UTC(),
	}, nil
}

// contractAdminPrincipal 返回具有合同系统超级管理员角色的测试主体。
// 参数：无。
// 返回值：admin 主体。
func contractAdminPrincipal() Principal {
	return Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}}
}

// TestGetImportAutoApproveSettingRequiresAdmin 验证读取运行开关只允许 admin：
// 普通角色即使持有其他权限也必须 403，且不读库。
// 参数：t 为测试上下文。
// 返回值：非 admin 被放行或触达仓储时失败。
func TestGetImportAutoApproveSettingRequiresAdmin(t *testing.T) {
	repo := &autoApproveSettingRepository{}
	service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", Roles: []string{"contract_specialist"}, Permissions: map[string]bool{"contract.import.approve": true}}

	setting, _, err := service.GetImportAutoApproveSetting(context.Background(), actor)
	if !errors.Is(err, ErrForbidden) || repo.getCalls != 0 || setting.Version != 0 {
		t.Fatalf("err=%v getCalls=%d setting=%+v", err, repo.getCalls, setting)
	}
}

// TestGetImportAutoApproveSettingReportsCapabilityAndTenantState 验证 GET 的两段信息：
// env 能力关闭时仍可读（返回 capability_enabled=false 与租户现有设置），
// 能力开启时返回租户设置；无记录按零值（version=0）。
// 参数：t 为测试上下文。
// 返回值：能力标记或设置内容不符时失败。
func TestGetImportAutoApproveSettingReportsCapabilityAndTenantState(t *testing.T) {
	actor := contractAdminPrincipal()
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	t.Run("capability-disabled-still-readable", func(t *testing.T) {
		repo := &autoApproveSettingRepository{setting: contract.ImportAutoApproveSetting{
			TenantID: "tenant-1", Enabled: true, Version: 3, UpdatedBy: "admin-9", UpdatedAt: now}}
		service := &Service{Repo: repo, ContractImportAutoApproveHistorical: false}
		setting, capability, err := service.GetImportAutoApproveSetting(context.Background(), actor)
		if err != nil || capability {
			t.Fatalf("err=%v capability=%t", err, capability)
		}
		if setting.Enabled != true || setting.Version != 3 || setting.UpdatedBy != "admin-9" || !setting.UpdatedAt.Equal(now) {
			t.Fatalf("setting=%+v", setting)
		}
	})

	t.Run("no-record-is-zero-value", func(t *testing.T) {
		service := &Service{Repo: &autoApproveSettingRepository{}, ContractImportAutoApproveHistorical: true}
		setting, capability, err := service.GetImportAutoApproveSetting(context.Background(), actor)
		if err != nil || !capability {
			t.Fatalf("err=%v capability=%t", err, capability)
		}
		if setting.Enabled || setting.Version != 0 || setting.UpdatedBy != "" || !setting.UpdatedAt.IsZero() {
			t.Fatalf("setting=%+v", setting)
		}
	})

	t.Run("capability-enabled-without-store-fails-closed", func(t *testing.T) {
		service := &Service{Repo: &recordingRepository{}, ContractImportAutoApproveHistorical: true}
		if _, _, err := service.GetImportAutoApproveSetting(context.Background(), actor); !errors.Is(err, ErrImportUnsupported) {
			t.Fatalf("err=%v, want ErrImportUnsupported", err)
		}
	})
}

// TestSetImportAutoApproveSettingRequiresAdmin 验证写入运行开关只允许 admin。
// 参数：t 为测试上下文。
// 返回值：非 admin 被放行或触达仓储时失败。
func TestSetImportAutoApproveSettingRequiresAdmin(t *testing.T) {
	repo := &autoApproveSettingRepository{}
	service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", Roles: []string{"finance_director"}}

	if _, err := service.SetImportAutoApproveSetting(context.Background(), actor, true, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err=%v, want ErrForbidden", err)
	}
	if repo.setCalls != 0 {
		t.Fatalf("setCalls=%d", repo.setCalls)
	}
}

// TestSetImportAutoApproveSettingRejectsWhenCapabilityDisabled 验证 env 能力关闭时
// 写入被专用错误拒绝（HTTP 422 IMPORT_AUTO_APPROVE_DISABLED），且不触达仓储。
// 参数：t 为测试上下文。
// 返回值：错误码不符或发生写入时失败。
func TestSetImportAutoApproveSettingRejectsWhenCapabilityDisabled(t *testing.T) {
	repo := &autoApproveSettingRepository{}
	service := &Service{Repo: repo, ContractImportAutoApproveHistorical: false}

	_, err := service.SetImportAutoApproveSetting(context.Background(), contractAdminPrincipal(), true, 0)
	if !errors.Is(err, ErrImportAutoApproveDisabled) {
		t.Fatalf("err=%v, want ErrImportAutoApproveDisabled", err)
	}
	if errors.Is(err, ErrImportUnsupported) {
		t.Fatal("能力关闭不应退化成通用不支持码")
	}
	if repo.setCalls != 0 {
		t.Fatalf("setCalls=%d", repo.setCalls)
	}
}

// TestSetImportAutoApproveSettingWritesTenantState 验证成功路径：
// 服务层只转发已认证主体的租户与用户，并把仓储返回的 version 原样交给 HTTP 层。
// 参数：t 为测试上下文。
// 返回值：转发参数或返回值不符时失败。
func TestSetImportAutoApproveSettingWritesTenantState(t *testing.T) {
	repo := &autoApproveSettingRepository{}
	service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}

	setting, err := service.SetImportAutoApproveSetting(context.Background(), contractAdminPrincipal(), true, 4)
	if err != nil {
		t.Fatal(err)
	}
	if repo.setCalls != 1 || repo.tenantID != "tenant-1" || !repo.enabled || repo.expected != 4 || repo.actorID != "admin-1" {
		t.Fatalf("calls=%d tenant=%q enabled=%t expected=%d actor=%q", repo.setCalls, repo.tenantID, repo.enabled, repo.expected, repo.actorID)
	}
	if !setting.Enabled || setting.Version != 5 || setting.UpdatedBy != "admin-1" || setting.UpdatedAt.IsZero() {
		t.Fatalf("setting=%+v", setting)
	}
}

// TestSetImportAutoApproveSettingPropagatesVersionConflict 验证版本冲突原样上抛
// （HTTP 层映射为 409 IMPORT_REVISION_CONFLICT），不被改写成成功或参数错误。
// 参数：t 为测试上下文。
// 返回值：错误未被识别为 ErrImportConflict 时失败。
func TestSetImportAutoApproveSettingPropagatesVersionConflict(t *testing.T) {
	repo := &autoApproveSettingRepository{setErr: ErrImportConflict}
	service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}

	if _, err := service.SetImportAutoApproveSetting(context.Background(), contractAdminPrincipal(), false, 2); !errors.Is(err, ErrImportConflict) {
		t.Fatalf("err=%v, want ErrImportConflict", err)
	}
}

// TestImportAutoApproveEnabledRequiresBothSwitches 验证生效判定组合：
// env 关闭时恒为 false 且不读库；env 开启时以租户设置为准；
// 读取错误上抛；仓储缺设置接口时失败关闭（ErrImportUnsupported），不静默跳过。
// 参数：t 为测试上下文。
// 返回值：任一组合与预期不符时失败。
func TestImportAutoApproveEnabledRequiresBothSwitches(t *testing.T) {
	ctx := context.Background()

	t.Run("environment-off-skips-store", func(t *testing.T) {
		repo := &autoApproveSettingRepository{setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 1}}
		service := &Service{Repo: repo, ContractImportAutoApproveHistorical: false}
		enabled, err := service.importAutoApproveEnabled(ctx, "tenant-1")
		if err != nil || enabled {
			t.Fatalf("enabled=%t err=%v", enabled, err)
		}
		if repo.getCalls != 0 {
			t.Fatalf("env 关闭仍然读库: getCalls=%d", repo.getCalls)
		}
	})

	t.Run("tenant-setting-off", func(t *testing.T) {
		repo := &autoApproveSettingRepository{setting: contract.ImportAutoApproveSetting{Enabled: false, Version: 2}}
		service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}
		enabled, err := service.importAutoApproveEnabled(ctx, "tenant-1")
		if err != nil || enabled || repo.getCalls != 1 || repo.tenantID != "tenant-1" {
			t.Fatalf("enabled=%t err=%v calls=%d tenant=%q", enabled, err, repo.getCalls, repo.tenantID)
		}
	})

	t.Run("tenant-setting-on", func(t *testing.T) {
		repo := &autoApproveSettingRepository{setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 2}}
		service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}
		enabled, err := service.importAutoApproveEnabled(ctx, "tenant-1")
		if err != nil || !enabled {
			t.Fatalf("enabled=%t err=%v", enabled, err)
		}
	})

	t.Run("read-error-propagates", func(t *testing.T) {
		readErr := errors.New("settings table unavailable")
		repo := &autoApproveSettingRepository{getErr: readErr}
		service := &Service{Repo: repo, ContractImportAutoApproveHistorical: true}
		enabled, err := service.importAutoApproveEnabled(ctx, "tenant-1")
		if enabled || !errors.Is(err, readErr) {
			t.Fatalf("enabled=%t err=%v, want read error", enabled, err)
		}
	})

	t.Run("missing-store-fails-closed", func(t *testing.T) {
		service := &Service{Repo: &recordingRepository{}, ContractImportAutoApproveHistorical: true}
		enabled, err := service.importAutoApproveEnabled(ctx, "tenant-1")
		if enabled || !errors.Is(err, ErrImportUnsupported) {
			t.Fatalf("enabled=%t err=%v, want ErrImportUnsupported", enabled, err)
		}
	})
}
