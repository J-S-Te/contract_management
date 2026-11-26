package mysql

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/testutil/scriptdb"
	"github.com/j-s-te/contract-management/internal/workflows"
)

// TestApprovalTasksReturnContractTitleAndApplicant 验证待办返回合同名称和申请人快照。
// 参数：t为脚本SQL测试上下文，不连接业务数据库。
// 返回值：无；字段映射、快照优先或租户限定缺失时失败。
func TestApprovalTasksReturnContractTitleAndApplicant(t *testing.T) {
	var query string
	var args []driver.NamedValue
	repo := grantSQLRepository(t, scriptdb.Step{
		Kind: "query", SQL: "con_approval_task AS t", SQLOut: &query, Args: &args,
		Names: []string{"approval_id", "contract_id", "contract_title", "node_id",
			"node_name", "assignee_user_id", "applicant_user_id", "applicant_display_name",
			"kind", "status", "node_index", "created_at"},
		Rows: [][]driver.Value{{"approval-a", "contract-a", "审批时的合同名称", "node-a",
			"部门审批", "user-a", "applicant-a", "审批提交人", "contract_approval", "active",
			int64(0), time.Now()}},
	})
	tasks, err := repo.ListTasks(context.Background(), "tenant-a", "user-a", 50)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	body, err := json.Marshal(tasks)
	if err != nil || !strings.Contains(string(body), `"contract_title":"审批时的合同名称"`) {
		t.Fatalf("合同名称未返回: %s err=%v", body, err)
	}
	if !strings.Contains(string(body), `"applicant_user_id":"applicant-a"`) ||
		!strings.Contains(string(body), `"applicant_display_name":"审批提交人"`) {
		t.Fatalf("申请人快照未返回: %s", body)
	}
	for _, part := range []string{
		"i.applicant_user_id", "i.applicant_display_name",
		"c.tenant_id = i.tenant_id", "s.tenant_id = i.tenant_id", "s.contract_id = i.contract_id",
		"JSON_EXTRACT(s.contract_json, '$.title')", "c.title", "i.tenant_id = ?",
		"i.status = ?", "t.assignee_user_id = ?", "t.status IN",
	} {
		if !strings.Contains(query, part) {
			t.Fatalf("查询缺少 %q: %s", part, query)
		}
	}
	if len(args) < 3 || args[0].Value != "tenant-a" || args[1].Value != "running" || args[2].Value != "user-a" {
		t.Fatalf("待办范围绑定错误: %+v", args)
	}
}

// TestPendingApprovalNotificationsIncludeContractTitle 验证待审批及催办通知持久化合同名称。
// 参数：t为脚本SQL测试上下文，覆盖个人与角色收件人。
// 返回值：无；正文缺名、快照范围错误或去重键改变时失败。
func TestPendingApprovalNotificationsIncludeContractTitle(t *testing.T) {
	for _, kind := range []string{"pending_approval", "approval_reminder", "approval_assigned"} {
		t.Run(kind, func(t *testing.T) {
			var query string
			var lookupArgs, insertArgs []driver.NamedValue
			repo := grantSQLRepository(t,
				scriptdb.Step{Kind: "query", SQL: "con_contract AS c", SQLOut: &query, Args: &lookupArgs,
					Names: []string{"title"}, Rows: [][]driver.Value{{"安全评估服务合同"}}},
				scriptdb.Step{Kind: "begin"},
				scriptdb.Step{Kind: "exec", SQL: "con_notification_outbox", Args: &insertArgs},
				scriptdb.Step{Kind: "commit"},
			)
			in := workflows.NotifyActivityInput{
				TenantID: "tenant-a", ApprovalID: "approval-a", ContractID: "contract-a", Type: kind,
				Recipients: []string{"user-a", "user-a"}, RoleRecipients: []string{"reviewer"},
				Title: "合同审批待处理", Content: "您有一项新的合同审批待办", DedupeKey: "stable-key",
			}
			if err := repo.CreateNotification(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{"s.tenant_id = c.tenant_id", "s.contract_id = c.id",
				"s.approval_id = ?", "c.tenant_id = ? AND c.id = ?", "JSON_EXTRACT"} {
				if !strings.Contains(query, part) {
					t.Fatalf("通知名称查询缺少范围 %q: %s", part, query)
				}
			}
			if len(lookupArgs) < 3 || lookupArgs[0].Value != "approval-a" ||
				lookupArgs[1].Value != "tenant-a" || lookupArgs[2].Value != "contract-a" {
				t.Fatalf("通知名称范围绑定错误: %+v", lookupArgs)
			}
			counts := map[string]int{}
			for _, arg := range insertArgs {
				if text, ok := arg.Value.(string); ok {
					counts[text]++
				}
			}
			if counts["合同名称：安全评估服务合同\n您有一项新的合同审批待办"] != 2 ||
				counts["stable-key"] != 2 || counts["user:user-a"] != 1 || counts["role:reviewer"] != 1 {
				t.Fatalf("通知正文、收件人或去重键不符: %+v", counts)
			}
		})
	}
}

// TestPendingApprovalNotificationTitleLookupFailure 验证名称查询失败时不写不完整的通知。
// 参数：t为脚本SQL测试上下文。
// 返回值：无；吞掉数据库错误或继续写通知时失败。
func TestPendingApprovalNotificationTitleLookupFailure(t *testing.T) {
	want := errors.New("notification contract lookup unavailable")
	repo := grantSQLRepository(t, scriptdb.Step{Kind: "query", SQL: "con_contract AS c", Err: want})
	err := repo.CreateNotification(context.Background(), workflows.NotifyActivityInput{
		TenantID: "tenant-a", ContractID: "contract-a", ApprovalID: "approval-a",
		Type: "pending_approval", Recipients: []string{"user-a"},
	})
	if !errors.Is(err, want) {
		t.Fatalf("通知查询错误未透传: %v", err)
	}
}

// TestOtherNotificationsPreserveContent 验证其他通知不查询合同且保留原正文。
// 参数：t为脚本SQL测试上下文。
// 返回值：无；引入额外查询或改变正文时失败。
func TestOtherNotificationsPreserveContent(t *testing.T) {
	var args []driver.NamedValue
	repo := grantSQLRepository(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "con_notification_outbox", Args: &args},
		scriptdb.Step{Kind: "commit"},
	)
	err := repo.CreateNotification(context.Background(), workflows.NotifyActivityInput{
		TenantID: "tenant-a", Type: "signing_shipped", Recipients: []string{"user-a"},
		Title: "合同寄出", Content: "原有通知正文", DedupeKey: "signing-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if arg.Value == "原有通知正文" {
			return
		}
	}
	t.Fatal("其他通知正文丢失")
}

// TestPendingApprovalNotificationWithoutRecipients 验证无有效收件人时不查合同或入队。
// 参数：t为脚本SQL测试上下文。
// 返回值：无；执行数据库操作或返回错误时失败。
func TestPendingApprovalNotificationWithoutRecipients(t *testing.T) {
	repo := grantSQLRepository(t)
	err := repo.CreateNotification(context.Background(), workflows.NotifyActivityInput{
		Type: "pending_approval", Recipients: []string{""},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestApprovalTaskNotificationFilters 验证通知定位在数量截断前限定合同和审批身份。
// 参数：t 为脚本SQL上下文，不连接业务数据库。
// 返回值：无；过滤丢失或租户及受派人范围被放宽时失败。
func TestApprovalTaskNotificationFilters(t *testing.T) {
	var query string
	var args []driver.NamedValue
	repo := grantSQLRepository(t, scriptdb.Step{Kind: "query", SQL: "con_approval_task AS t",
		SQLOut: &query, Args: &args, Names: []string{"approval_id"}})
	_, err := repo.ListTasks(context.Background(), "tenant-a", "user-a", 50,
		approval.ListFilter{ContractID: "contract-a", ApprovalID: "approval-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"i.tenant_id = ?", "t.assignee_user_id = ?", "i.contract_id = ?", "t.approval_id = ?"} {
		if !strings.Contains(query, part) {
			t.Fatalf("missing %s: %s", part, query)
		}
	}
	values := map[any]bool{}
	for _, arg := range args {
		values[arg.Value] = true
	}
	if !values["contract-a"] || !values["approval-a"] {
		t.Fatalf("args=%+v", args)
	}
}
