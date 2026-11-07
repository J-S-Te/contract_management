package application

import (
	"context"
	"errors"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ErrImportAutoApproveDisabled 表示 env 能力开关（CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL）
// 未开启，租户级运行开关因此不可写。
// 与 ErrImportUnsupported 分开：前者是「部署层能力未装配」，后者是「仓储缺能力」，
// HTTP 层据此返回明确错误码，前端不需要靠猜。
var ErrImportAutoApproveDisabled = errors.New("IMPORT_AUTO_APPROVE_DISABLED")

// ContractImportAutoApproveSettingStore 是租户级自动审批运行开关的持久化能力。
// 与 ContractImportAutoApproveStore（真正执行自动确认）分开：读写开关只需要
// 一张小表，缺失实现时按能力未开放失败关闭，不能静默退化成「永远关闭」。
type ContractImportAutoApproveSettingStore interface {
	// GetImportAutoApproveSetting 读取租户设置；没有记录时返回零值（Version=0）且不报错。
	GetImportAutoApproveSetting(ctx context.Context, tenantID string) (contract.ImportAutoApproveSetting, error)
	// SetImportAutoApproveSetting 以 expectedVersion 做乐观并发写入，成功时 Version 已 +1。
	// 无记录时 expectedVersion 必须为 0；版本不符返回 ErrImportConflict。
	SetImportAutoApproveSetting(ctx context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error)
}

// isContractAdmin 判断主体是否为合同系统超级管理员。
// 运行开关直接决定是否跳过人工会签，因此只认显式 admin 角色，不接受权限位替代。
// 参数：actor 为已认证主体。
// 返回值：角色列表包含 admin 时为 true。
func isContractAdmin(actor Principal) bool {
	return hasRole(actor, "admin")
}

// GetImportAutoApproveSetting 读取当前租户的历史导入自动审批运行开关。
// 参数：ctx 为请求上下文，actor 为已认证主体（仅 admin 可读）。
// 返回值：租户设置、env 能力开关状态、错误；无记录时设置按零值返回。
func (s *Service) GetImportAutoApproveSetting(ctx context.Context, actor Principal) (contract.ImportAutoApproveSetting, bool, error) {
	capability := s.ContractImportAutoApproveHistorical
	if !isContractAdmin(actor) {
		return contract.ImportAutoApproveSetting{}, capability, ErrForbidden
	}
	store, ok := s.Repo.(ContractImportAutoApproveSettingStore)
	if !ok {
		// 能力关闭时接口仍要能回答「不可用」，不因仓储未装配而变成 500；
		// 能力开启却没有设置表则是装配缺口，必须失败关闭而不是假装「关闭」。
		if capability {
			return contract.ImportAutoApproveSetting{}, capability, ErrImportUnsupported
		}
		return contract.ImportAutoApproveSetting{}, capability, nil
	}
	setting, err := store.GetImportAutoApproveSetting(ctx, actor.TenantID)
	if err != nil {
		return contract.ImportAutoApproveSetting{}, capability, err
	}
	return setting, capability, nil
}

// SetImportAutoApproveSetting 写入当前租户的历史导入自动审批运行开关。
// 参数：ctx 为请求上下文，actor 为已认证主体（仅 admin 可写），enabled 为目标状态，
// expectedVersion 为客户端读到的版本（首次写入为 0）。
// 返回值：写入后的设置；能力关闭、非 admin、版本冲突或存储错误。
func (s *Service) SetImportAutoApproveSetting(ctx context.Context, actor Principal, enabled bool, expectedVersion uint64) (contract.ImportAutoApproveSetting, error) {
	if !isContractAdmin(actor) {
		return contract.ImportAutoApproveSetting{}, ErrForbidden
	}
	if !s.ContractImportAutoApproveHistorical {
		return contract.ImportAutoApproveSetting{}, ErrImportAutoApproveDisabled
	}
	store, ok := s.Repo.(ContractImportAutoApproveSettingStore)
	if !ok {
		return contract.ImportAutoApproveSetting{}, ErrImportUnsupported
	}
	return store.SetImportAutoApproveSetting(ctx, actor.TenantID, enabled, expectedVersion, actor.UserID)
}

// importAutoApproveLive 读取「此刻」的生效判定与设置版本。
//
// 只用于两种场景：提交事务外预判是否需要 Temporal，以及确认台账未固化策略的
// 旧数据执行期回退判定。已固化策略的确认不得再调用本函数——否则执行期会被
// 开关变更改道，重新制造提交/执行口径不一致。
//
// 参数：ctx 为执行上下文，tenantID 为批次所属租户。
// 返回值：是否生效、设置版本（env 关闭时为 0）；仓储缺能力返回 ErrImportUnsupported
// （失败关闭），读取错误原样上抛让调度器把该项记为可重试依赖故障。
func (s *Service) importAutoApproveLive(ctx context.Context, tenantID string) (bool, uint64, error) {
	if !s.ContractImportAutoApproveHistorical {
		return false, 0, nil
	}
	store, ok := s.Repo.(ContractImportAutoApproveSettingStore)
	if !ok {
		return false, 0, ErrImportUnsupported
	}
	setting, err := store.GetImportAutoApproveSetting(ctx, tenantID)
	if err != nil {
		return false, 0, err
	}
	return setting.Enabled, setting.Version, nil
}

// ImportAutoApprovePolicy 是自动确认执行期的策略快照。
//
// Auto 决定是否执行自动确认；Version 是要写进合成确认台账的设置版本；
// AllowLegacyUpgrade 只在「确认台账存在但尚未固化策略」的旧数据上为真，
// 用于把提交时已自动批准、缺少新字段的历史批次就地升级，而不放宽其他状态。
type ImportAutoApprovePolicy struct {
	Auto               bool
	Version            uint64
	AllowLegacyUpgrade bool
}

// importAutoApprovePolicy 解析本批次执行期应使用的策略。
//
// 三条分支对应三种数据形态：
//   - 已固化策略（PolicyPinned）：只认 pin，完全不读 env/库，开关后续变更不影响在途批次；
//   - 无确认台账（Confirmation==nil）：跟随 live，合成本次开立所需的确认形状；
//   - 有确认台账但未固化（旧数据）：跟随 live 并允许严格护栏下的就地升级。
//
// 参数：ctx 为执行上下文，b 为已认领批次。
// 返回值：策略快照或读取/能力错误（由调度器按可重试依赖故障处理）。
func (s *Service) importAutoApprovePolicy(ctx context.Context, b contract.ImportBatch) (ImportAutoApprovePolicy, error) {
	if b.Confirmation != nil && b.Confirmation.PolicyPinned() {
		return ImportAutoApprovePolicy{Auto: b.Confirmation.AutoApproved(), Version: b.Confirmation.PolicyVersion}, nil
	}
	live, version, err := s.importAutoApproveLive(ctx, b.TenantID)
	if err != nil {
		return ImportAutoApprovePolicy{}, err
	}
	if b.Confirmation == nil {
		return ImportAutoApprovePolicy{Auto: live, Version: version}, nil
	}
	return ImportAutoApprovePolicy{Auto: live, Version: version, AllowLegacyUpgrade: live}, nil
}

// ContractImportAutoApproveStore 由仓储实现历史导入的自动确认写入。
// 该能力单独成接口：仓储缺失时按 IMPORT_POLICY_UNSUPPORTED 拒绝，而不是退化成
// 半自动状态（合同已开立但永不归档）。
type ContractImportAutoApproveStore interface {
	// AutoConfirmHistoricalImportItem 在单事务内把一条历史开立项确认为声明终态。
	// 参数：ctx 为上下文，tenantID/batchID/itemID 定位批次与单项，roles 为自动确认
	// 记入确认台账的审批角色（用于与人工会签保持同一可审计形状），policy 为执行期
	// 策略快照（决定是否允许旧数据升级与写入哪个设置版本）。
	// 返回值：冲突、清单或存储错误；重复调用必须幂等。
	AutoConfirmHistoricalImportItem(ctx context.Context, tenantID, batchID, itemID string, roles []string, policy ImportAutoApprovePolicy) error
}

// autoConfirmHistoricalImportItem 在 CRM 开立成功后自动确认并归档一条历史项。
//
// 为什么挂在开立编排之后：历史合同的 CRM 客户/商机开立完成时，合同行刚由
// CommitImportDraft 写成 version=1、SourceFileStatus=READY、OriginKind=IMPORTED，
// 正是历史确认（confirmHistoricalItem）要求的前置基线。此时确认既能复用既有的
// 编号分配与审计写入，又避免人工会签再等一轮。
//
// 参数：ctx 为执行上下文，b 为已认领批次（提供租户与批次围栏），itemID 为单项 ID，
// policy 为调用点已解析的策略快照（原样透传给仓储，不在这里二次判定）。
// 返回值：仓储缺失返回 ErrImportUnsupported，其余为仓储错误。
func (s *Service) autoConfirmHistoricalImportItem(ctx context.Context, b contract.ImportBatch, itemID string, policy ImportAutoApprovePolicy) error {
	store, ok := s.Repo.(ContractImportAutoApproveStore)
	if !ok {
		return ErrImportUnsupported
	}
	return store.AutoConfirmHistoricalImportItem(ctx, b.TenantID, b.ID, itemID, s.ContractImportConfirmationRoles, policy)
}
