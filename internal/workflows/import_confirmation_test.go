package workflows

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"go.temporal.io/sdk/testsuite"
	"testing"
)

type importConfirmationMemoryStore struct {
	memoryStore
	states []string
	calls  int
}

// AdvanceImportConfirmation 提供SDK测试的确定性持久状态序列。
// 参数：ctx和in为工作流活动参数。
// 返回值：模拟Activity状态；不冒充真实Temporal服务证明。
func (s *importConfirmationMemoryStore) AdvanceImportConfirmation(ctx context.Context, in ImportConfirmationInput) (string, error) {
	state := s.states[min(s.calls, len(s.states)-1)]
	s.calls++
	return state, nil
}

// TestImportConfirmationWorkflowTerminalStates 验证独立Workflow仅按持久Activity终态收口。
// 参数：t为SDK测试环境。
// 返回值：无；批准、驳回、撤回和到期行为不符时失败。
func TestImportConfirmationWorkflowTerminalStates(t *testing.T) {
	for _, state := range []string{"APPROVED", "REJECTED", "WITHDRAWN", "EXPIRED"} {
		t.Run(state, func(t *testing.T) {
			store := &importConfirmationMemoryStore{states: []string{"STARTING", "PENDING", state}}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivity(&Activities{Store: store})
			env.ExecuteWorkflow(ImportConfirmationWorkflow, ImportConfirmationInput{TenantID: "test", BatchID: "batch", Digest: "digest", Revision: 1})
			var result string
			if e := env.GetWorkflowResult(&result); e != nil || result != state || store.calls != 3 {
				t.Fatalf("state=%s result=%s calls=%d err=%v", state, result, store.calls, e)
			}
		})
	}
}

type importRecoveringLicense struct{ attempts int }

// Check 模拟明确许可暂停后恢复，不绕过任何实际业务许可。
// 参数：ctx及op为被测活动请求。
// 返回值：前两次拒绝，其后允许，供SDK虚拟时间测试。
func (l *importRecoveringLicense) Check(ctx context.Context, op core.Operation) error {
	l.attempts++
	if l.attempts <= 2 {
		return core.ErrDenied
	}
	return nil
}

// TestImportConfirmationLicensePauseRecovers 验证历史确认在许可恢复后继续，不丢失持久工作流。
// 参数：t为SDK测试环境。
// 返回值：无；许可暂停导致工作流永久失败时失败。
func TestImportConfirmationLicensePauseRecovers(t *testing.T) {
	store := &importConfirmationMemoryStore{states: []string{"APPROVED"}}
	license := &importRecoveringLicense{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store, LicenseGate: license})
	env.ExecuteWorkflow(ImportConfirmationWorkflow, ImportConfirmationInput{TenantID: "test", BatchID: "batch", Digest: "digest", Revision: 1})
	var state string
	if e := env.GetWorkflowResult(&state); e != nil || state != "APPROVED" || license.attempts != 3 {
		t.Fatalf("license recovery state=%s attempts=%d err=%v", state, license.attempts, e)
	}
}
