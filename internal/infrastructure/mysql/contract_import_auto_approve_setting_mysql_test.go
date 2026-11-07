package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/oklog/ulid/v2"
)

// autoApproveSettingCleanup 注册本用例写入的租户数据清理。
// 参数：t 为测试上下文，r 为隔离库仓储，tenants 为已创建租户。
// 返回值：无；清理失败只记录到测试错误。
func autoApproveSettingCleanup(t *testing.T, r *Repository, tenants ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tenant := range tenants {
			for _, table := range []string{"con_import_auto_approve_setting", "con_audit_outbox"} {
				if err := r.db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
					t.Error(err)
				}
			}
		}
	})
}

// TestImportAutoApproveSettingDefaultsAndVersionedWrites 验证运行开关的持久化契约：
// 无记录按零值（version=0）；expected_version=0 首次插入 version=1；随后写入 version+1；
// 旧版本写入返回冲突；每次成功写入同事务落一条审计，payload 只含开关前后状态与版本。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：默认值、版本推进、冲突或审计不符时失败。
func TestImportAutoApproveSettingDefaultsAndVersionedWrites(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	autoApproveSettingCleanup(t, r, tenant)
	ctx := context.Background()
	actor := ulid.Make().String()

	// 默认零值：没有记录不报错，version=0 表示「运行开关未开启」。
	setting, err := r.GetImportAutoApproveSetting(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if setting.Enabled || setting.Version != 0 || setting.UpdatedBy != "" || !setting.UpdatedAt.IsZero() {
		t.Fatalf("默认设置非零值: %+v", setting)
	}

	// 首次写入：expected_version=0 → insert version=1。
	created, err := r.SetImportAutoApproveSetting(ctx, tenant, true, 0, actor)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Enabled || created.Version != 1 || created.UpdatedBy != actor || created.UpdatedAt.IsZero() {
		t.Fatalf("首次写入=%+v", created)
	}
	if created.TenantID != tenant {
		t.Fatalf("tenant=%q", created.TenantID)
	}

	// 第二次写入：expected_version=1 → version=2，状态可回写为关闭。
	updated, err := r.SetImportAutoApproveSetting(ctx, tenant, false, 1, actor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.Version != 2 || updated.UpdatedBy != actor {
		t.Fatalf("第二次写入=%+v", updated)
	}
	read, err := r.GetImportAutoApproveSetting(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if read.Enabled || read.Version != 2 || read.UpdatedBy != actor {
		t.Fatalf("回读=%+v", read)
	}

	// 旧版本冲突：不得覆盖 version=2，也不得产生第三条审计。
	if _, err = r.SetImportAutoApproveSetting(ctx, tenant, true, 1, actor); !errors.Is(err, application.ErrImportConflict) {
		t.Fatalf("过期版本 err=%v, want ErrImportConflict", err)
	}
	read, err = r.GetImportAutoApproveSetting(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if read.Version != 2 || read.Enabled {
		t.Fatalf("冲突写入改变了设置: %+v", read)
	}

	// 审计：两条成功写入各一条，动作/资源/操作者可核对，payload 不含敏感信息。
	var entries []auditOutboxRecord
	if err = r.db.Where("tenant_id=? AND action=?", tenant, importAutoApproveSettingAuditAction).
		Order("created_at ASC").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("审计条数=%d, want 2", len(entries))
	}
	wantPayloads := []map[string]any{
		{"enabled": true, "previous_enabled": false, "version": float64(1)},
		{"enabled": false, "previous_enabled": true, "version": float64(2)},
	}
	for index, entry := range entries {
		if entry.ResourceType != "contract_import_auto_approve" || entry.ResourceID != tenant {
			t.Fatalf("审计资源=%s/%s", entry.ResourceType, entry.ResourceID)
		}
		if entry.ActorID == nil || *entry.ActorID != actor {
			t.Fatalf("审计操作者=%v", entry.ActorID)
		}
		var payload map[string]any
		if err = json.Unmarshal(entry.PayloadJSON, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 3 || payload["enabled"] != wantPayloads[index]["enabled"] ||
			payload["previous_enabled"] != wantPayloads[index]["previous_enabled"] ||
			payload["version"] != wantPayloads[index]["version"] {
			t.Fatalf("第 %d 条审计 payload=%v", index+1, payload)
		}
	}
}

// TestImportAutoApproveSettingIsTenantScoped 验证租户隔离：
// 一个租户写入不影响另一个租户；对无记录租户提交非零 expected_version 按冲突拒绝，
// 且不得留下半行数据（首次写入必须从 version=0 起）。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：跨租户串写或冲突写入落库时失败。
func TestImportAutoApproveSettingIsTenantScoped(t *testing.T) {
	r, tenantA := draftE2ERepository(t)
	tenantB := ulid.Make().String()
	autoApproveSettingCleanup(t, r, tenantA, tenantB)
	ctx := context.Background()
	actor := ulid.Make().String()

	if _, err := r.SetImportAutoApproveSetting(ctx, tenantA, true, 0, actor); err != nil {
		t.Fatal(err)
	}
	settingB, err := r.GetImportAutoApproveSetting(ctx, tenantB)
	if err != nil {
		t.Fatal(err)
	}
	if settingB.Enabled || settingB.Version != 0 {
		t.Fatalf("租户 B 读到租户 A 的设置: %+v", settingB)
	}
	if _, err = r.SetImportAutoApproveSetting(ctx, tenantB, false, 3, actor); !errors.Is(err, application.ErrImportConflict) {
		t.Fatalf("无记录租户提交 expected_version=3 err=%v, want ErrImportConflict", err)
	}
	var count int64
	if err = r.db.Model(&importAutoApproveSettingRecord{}).Where("tenant_id=?", tenantB).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("冲突写入留下 %d 行", count)
	}
	// 隔离库清理时也校验更新时间可读（DATETIME(6) 写入/回读一致）。
	if _, err = r.SetImportAutoApproveSetting(ctx, tenantB, true, 0, actor); err != nil {
		t.Fatal(err)
	}
	settingB, err = r.GetImportAutoApproveSetting(ctx, tenantB)
	if err != nil {
		t.Fatal(err)
	}
	if !settingB.Enabled || settingB.Version != 1 || settingB.UpdatedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("租户 B 写入=%+v", settingB)
	}
}
