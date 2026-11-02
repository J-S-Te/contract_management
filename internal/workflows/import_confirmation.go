package workflows

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"time"
)

const ImportConfirmationWorkflowName = "contract-import-confirmation"

type ImportConfirmationInput struct {
	TenantID, BatchID, Digest string
	Revision                  uint64
}
type ImportConfirmationStore interface {
	AdvanceImportConfirmation(context.Context, ImportConfirmationInput) (string, error)
}

// ImportConfirmationWorkflow 独立调度历史确认，不生成历史普通审批事实。
// 参数：ctx为Temporal上下文，in为冻结修订和清单摘要。
// 返回值：持久确认终态或明确工作流错误。
func ImportConfirmationWorkflow(ctx workflow.Context, in ImportConfirmationInput) (string, error) {
	if in.BatchID == "" || in.Digest == "" || in.Revision == 0 {
		return "", temporal.NewNonRetryableApplicationError("invalid history identity", "IMPORT_CONFLICT", nil)
	}
	// 无限重试预算只留给数据库/网络等执行故障：确定性业务冲突（编号耗尽、编号规则非法、
	// 基线冲突等）已由活动层包装为 NonRetryable IMPORT_CONFLICT，Temporal 不会重试，
	// 批次不会被无限重试卡在中间态。
	opts := workflow.ActivityOptions{StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute, MaximumAttempts: 0}}
	ctx = workflow.WithActivityOptions(ctx, opts)
	for {
		var state string
		if err := executeLicensedActivity(ctx, ctx, "AdvanceImportConfirmation", in, &state); err != nil {
			return state, err
		}
		if state == "APPROVED" || state == "REJECTED" || state == "WITHDRAWN" || state == "EXPIRED" {
			return state, nil
		}
		if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
			return state, err
		}
	}
}

// AdvanceImportConfirmation 复核并执行权威台账中的受信命令。
// 参数：ctx为活动上下文，in为独立确认身份。
// 返回值：当前确认状态或存储、许可和不可重试冲突。
func (a *Activities) AdvanceImportConfirmation(ctx context.Context, in ImportConfirmationInput) (string, error) {
	if err := a.checkLicense(ctx); err != nil {
		return "", err
	}
	store, ok := a.Store.(ImportConfirmationStore)
	if !ok {
		return "", temporal.NewNonRetryableApplicationError("history store unavailable", "IMPORT_UNSUPPORTED", nil)
	}
	state, err := store.AdvanceImportConfirmation(ctx, in)
	return state, importConfirmationError(err)
}

// importConflictErrorType 是历史导入确认的确定性业务冲突错误码，沿用本文件既有风格。
const importConflictErrorType = "IMPORT_CONFLICT"

// contractBusinessConflictErrorType 必须与 activities.go 中 criticalActivityError 的错误码一致：
// 导入路径复用同一份业务冲突判定，只更换错误码与面向运维的文案。
const contractBusinessConflictErrorType = "CONTRACT_BUSINESS_CONFLICT"

// importConflictHints 给导入确认的确定性业务冲突提供运维可读的中文文案。
// 匹配顺序按 errors.Is 从具体到宽泛，未命中时使用通用文案加原始错误。
var importConflictHints = []struct {
	target  error
	message string
}{
	{
		target: contract.ErrNumberExhausted,
		message: "历史导入取号失败：当年合同编号流水已耗尽，或连续顺延后仍被全系统编号登记表占用；" +
			"请先调整系统级合同编号规则的流水位宽（或更换前缀），再重新发起本批次确认；" +
			"若规则受限，请联系技术核对 con_contract_number_registry 中的历史占用。",
	},
	{
		target: contract.ErrInvalidNumberRule,
		message: "历史导入取号失败：系统级合同编号规则不合法（前缀、后缀、年份段或流水位宽不满足约束）；" +
			"请先在合同编号规则设置中修正并保存，再重新发起本批次确认。",
	},
	{
		target: contract.ErrNumberConflict,
		message: "历史导入写号失败：分配到的系统编号已被本租户的另一份合同占用（uk_con_contract_number 唯一键冲突）；" +
			"重试无法自动恢复，请联系技术执行合同编号审计（cmd/contract-number-audit）受控更正数据后重新发起本批次确认。",
	},
	{
		target: contract.ErrImportManifest,
		message: "历史导入基线冲突：批次冻结摘要、来源代次或合同原件事实与本批次不一致；" +
			"本批次不能自动确认，请重新发起导入确认，必要时联系技术核对批次与合同状态。",
	},
}

// importConfirmationError 把导入确认活动的失败翻译成工作流可见的错误。
//
// 「哪些错误属于确定性业务冲突」完全复用审批路径的 criticalActivityError，
// 不再在本文件维护第二份错误清单：审批与导入对同一批错误必须给出同一种分类，
// 后续 criticalActivityError 新增分类（如新的编号冲突业务错误）时导入路径自动跟上。
// 导入路径只是把错误码换成 IMPORT_CONFLICT，并用中文文案说明运维该怎么做，
// 因为历史导入是长事务批次，无限重试只会让批次永久停在中间态。
// 数据库、网络等执行故障保持原样，继续由工作流的无限重试预算处理。
func importConfirmationError(err error) error {
	if err == nil {
		return nil
	}
	// ErrImportManifest 只出现在导入路径，不在 criticalActivityError 的分类清单里。
	if !errors.Is(err, contract.ErrImportManifest) && !isContractBusinessConflict(err) {
		return err
	}
	message := "历史导入确认遇到确定性业务冲突：" + err.Error() +
		"；本批次不会自动重试，请核对批次状态与合同当前版本后重新发起确认。"
	for _, hint := range importConflictHints {
		if errors.Is(err, hint.target) {
			message = hint.message
			break
		}
	}
	return temporal.NewNonRetryableApplicationError(message, importConflictErrorType, err)
}

// isContractBusinessConflict 复用 criticalActivityError 的判定结果回答"是否业务冲突"，
// 避免在工作流包里出现第二份容易漂移的 errors.Is 清单。
func isContractBusinessConflict(err error) bool {
	var classified *temporal.ApplicationError
	return errors.As(criticalActivityError(err), &classified) &&
		classified.Type() == contractBusinessConflictErrorType
}
