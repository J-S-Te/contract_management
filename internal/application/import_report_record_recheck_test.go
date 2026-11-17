package application

import (
	"context"
	"errors"
	"testing"
)

// TestRestoredReportRecheckPreservesOriginalIdentity 用途：验证人工统计核对不新建发布版本或命令身份。
// 参数：t 为服务测试上下文，存储替身只执行真实版本围栏和业务回调。
// 返回值：无；原命令、来源版本、发生时间漂移或越过排队直接派生时失败。
func TestRestoredReportRecheckPreservesOriginalIdentity(t *testing.T) {
	r, service, actor := rejectedProjectFixture(t)
	actor.Roles = []string{"admin"}
	task := &r.batch.Items[0].Targets[0]
	task.Target, task.ErrorCode = "REPORT", "CONTRACT_RECORD_DELETED"
	task.CommandID, task.RequestPayload = "0123456789abcdef0123456789abcdef", ""
	task.RecipientHTTPStatus, task.RecipientCode = 0, ""
	before := *task
	result, err := service.ReleaseImportItem(context.Background(), actor, "item", "REPORT", task.Version, "RELEASE")
	if err != nil {
		t.Fatal(err)
	}
	got := result.Items[0].Targets[0]
	if got.State != "QUEUED" || got.ErrorCode != "" || got.CommandID != before.CommandID ||
		got.ReleaseVersion != before.ReleaseVersion || !got.OccurredAt.Equal(before.OccurredAt) ||
		got.Epoch != before.Epoch+1 || got.Version != before.Version+1 || got.RecoveryRequestedBy != actor.UserID {
		t.Fatalf("recheck=%+v", got)
	}
	if result.Items[0].SourceRevision != r.batch.Items[0].SourceRevision {
		t.Fatal("recheck changed source baseline")
	}
}

// TestReportRecheckRequiresExplicitAdminAndDeletedReason 用途：验证新增入口不放开其他角色或未知拒绝。
// 参数：t 为测试上下文，各场景只改变角色或明确终态原因。
// 返回值：无；非管理员或其他 BLOCKED 被放行时失败。
func TestReportRecheckRequiresExplicitAdminAndDeletedReason(t *testing.T) {
	for _, tc := range []struct {
		name, role, reason string
		want               error
	}{
		{"non_admin", "finance_director", "CONTRACT_RECORD_DELETED", ErrForbidden},
		{"other_blocker", "admin", "IMPORT_TARGET_RECEIPT_INVALID", ErrImportUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, service, actor := rejectedProjectFixture(t)
			actor.Roles = []string{tc.role}
			task := &r.batch.Items[0].Targets[0]
			task.Target, task.ErrorCode = "REPORT", tc.reason
			_, err := service.ReleaseImportItem(context.Background(), actor, "item", "REPORT", task.Version, "RELEASE")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if r.batch.Items[0].Targets[0].State != "BLOCKED" {
				t.Fatal("denied request changed task")
			}
		})
	}
}
