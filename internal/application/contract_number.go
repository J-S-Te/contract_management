package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ContractNumberRuleStore 持久化系统级编号规则与按年度重置的全系统流水水位。
// 仓储未实现该能力时编号规则接口必须安全失败，不能用默认值冒充已保存配置。
//
// 流水只是候选号来源；单号在全系统唯一（跨租户也不重号）由取号事务里的
// con_contract_number_registry 主键强制，因此这里读到水位不等于“下一个单号一定可用”。
type ContractNumberRuleStore interface {
	// GetContractNumberRule 返回全局单行规则；规则行缺失时 found=false。
	GetContractNumberRule(context.Context) (contract.ContractNumberRule, bool, error)
	// SaveContractNumberRule 全量覆盖全局单行规则。
	SaveContractNumberRule(context.Context, contract.ContractNumberRule) error
	// ContractNumberSequenceValue 返回指定规则桶在给定年度的全系统已用流水值；
	// ruleKey 由 contract.ContractNumberRuleKey 计算，未使用过为 0。
	ContractNumberSequenceValue(context.Context, string, int) (uint64, error)
}

// ContractNumberRegistryLookup 是保存规则前干跑占号检查所需的只读能力。
//
// 刻意不并入 ContractNumberRuleStore：不改动既有 GET/PUT 的仓储契约，也不要求
// 每个测试替身都实现它。SaveContractNumberRule 只在仓储同时实现本接口时才做干跑
// （生产仓储 internal/infrastructure/mysql.Repository 已实现），未实现则保持原行为。
type ContractNumberRegistryLookup interface {
	// ContractNumberRegistryContains 判断候选单号是否已被全系统登记表占用；
	// 该列排序规则大小写不敏感，因此“只改前缀大小写”的规则会命中历史编号。
	ContractNumberRegistryContains(context.Context, string) (bool, error)
}

// ContractNumberRuleView 是编号规则的读取视图。
// Sample 是按当前配置与本年度的规则桶水位推算的下一个示例单号，仅用于界面展示，
// 不消耗流水、也不预占登记表；CurrentValue 是当前规则桶在 SeqYear 已分配的全系统流水值
// （不区分租户）。登记表里可能还有历史编号，实际签发号可能大于 Sample 所示。
// Sample 为空时 SampleUnavailableReason 说明原因，界面必须展示原因而不是编一个假号。
type ContractNumberRuleView struct {
	Prefix                  string    `json:"prefix"`
	YearSegment             string    `json:"year_segment"`
	SeqWidth                int       `json:"seq_width"`
	Suffix                  string    `json:"suffix"`
	Sample                  string    `json:"sample"`
	SampleUnavailableReason string    `json:"sample_unavailable_reason,omitempty"`
	SeqYear                 int       `json:"seq_year"`
	CurrentValue            uint64    `json:"current_value"`
	UpdatedBy               string    `json:"updated_by"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// GetContractNumberRule 返回系统级编号规则与当前规则桶当年全系统流水水位。
// 读取沿用 contract.read：普通用户需要看到当前生效的前缀与示例单号，
// 返回内容只有规则字段、当年水位和最后修改人/时间，不含其它租户数据。
// 参数：ctx 为上下文，actor 为已认证主体。
// 返回值：规则视图，或权限/依赖错误。
func (s *Service) GetContractNumberRule(ctx context.Context, actor Principal) (ContractNumberRuleView, error) {
	if !hasPermissionOrAdmin(actor, "contract.read") {
		return ContractNumberRuleView{}, ErrForbidden
	}
	return s.contractNumberRuleView(ctx)
}

// UpdateContractNumberRule 全量覆盖系统级编号规则。
// 已生成的合同编号不回填、不重算，仅影响后续取号。
//
// 该规则是全系统单行配置（无 tenant_id），改动会影响所有租户后续签发，
// 因此写入只认专用高权权限码 contract.number_rule.manage，并刻意不提供
// admin 角色旁路：否则任一租户的合同管理员都能改全系统前缀。
// 参数：ctx 为上下文，actor 为已认证主体，rule 为请求体中的规则。
// 返回值：保存后的规则视图，或权限、校验、依赖错误。
func (s *Service) UpdateContractNumberRule(ctx context.Context, actor Principal, rule contract.ContractNumberRule) (ContractNumberRuleView, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return ContractNumberRuleView{}, err
	}
	if !actor.Has("contract.number_rule.manage") {
		return ContractNumberRuleView{}, ErrForbidden
	}
	normalized, err := contract.NormalizeContractNumberRule(rule)
	if err != nil {
		return ContractNumberRuleView{}, fmt.Errorf("%w: %s", ErrValidation, contractNumberRuleReason(err))
	}
	store, err := s.contractNumberRuleStore()
	if err != nil {
		return ContractNumberRuleView{}, err
	}
	normalized.UpdatedBy = strings.TrimSpace(actor.UserID)
	normalized.UpdatedAt = time.Now().UTC()
	if err := s.checkBusinessLicense(ctx); err != nil {
		return ContractNumberRuleView{}, err
	}
	// 保存前干跑一次“新规则第一个候选号是否已被全系统登记”：只改前缀/后缀大小写会换到
	// 新流水桶从 1 开始，而候选号在登记表主键（utf8mb4_unicode_ci，大小写不敏感）上仍与
	// 历史号相等，保存后会在 50 次顺延后让全系统停发。这里提前拒绝并提示更换前缀。
	if err := s.checkContractNumberRuleIssuable(ctx, store, normalized); err != nil {
		return ContractNumberRuleView{}, err
	}
	if err := store.SaveContractNumberRule(ctx, normalized); err != nil {
		return ContractNumberRuleView{}, err
	}
	return s.contractNumberRuleView(ctx)
}

// checkContractNumberRuleIssuable 干跑校验新规则的第一个候选号是否已被登记表占用。
//
// 只做一次只读查询，不消耗流水、不预占号码：候选号 = 新规则桶当前水位 + 1。
// 仓储未实现 ContractNumberRegistryLookup 时跳过该检查（不改动既有 PUT 契约）。
// 参数：ctx 为上下文，store 为规则仓储，rule 为已归一化的新规则。
// 返回值：命中历史号返回包装 ErrValidation 的中文原因（HTTP 422），其它错误原样返回。
func (s *Service) checkContractNumberRuleIssuable(ctx context.Context, store ContractNumberRuleStore, rule contract.ContractNumberRule) error {
	lookup, ok := store.(ContractNumberRegistryLookup)
	if !ok || lookup == nil {
		return nil
	}
	ruleKey, err := contract.ContractNumberRuleKey(rule)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrValidation, contractNumberRuleReason(err))
	}
	// 干跑查的必须是真实取号会命中的那个桶：取号按北京日历日取年，这里用 UTC 取年会
	// 在北京时间 1 月 1 日 00:00–08:00 查错年份的桶，导致查重结果与签发不一致。
	year := businessclock.YearOf(time.Now())
	current, err := store.ContractNumberSequenceValue(ctx, ruleKey, year)
	if err != nil {
		return err
	}
	// 水位已到位宽上限时拼不出候选号：这种情况由取号阶段报“流水耗尽”，
	// 与“号码被历史占用”不是同一类问题，这里不拦保存。
	candidate, err := contract.BuildContractNumber(rule, year, current+1)
	if err != nil {
		return nil
	}
	occupied, err := lookup.ContractNumberRegistryContains(ctx, candidate)
	if err != nil {
		return err
	}
	if occupied {
		return fmt.Errorf("%w: 新规则算出的下一个单号 %s 已被全系统登记表占用（号码比对不区分大小写），"+
			"直接保存会让取号在顺延上限后失败。请更换前缀/后缀，或先执行 cmd/contract-number-audit 核对并按受控流程清理该编号后再保存",
			ErrValidation, candidate)
	}
	return nil
}

// contractNumberRuleView 组装规则读取视图。
//
// 规则行缺失时：前缀/年份段/位宽仍展示迁移种子默认值（界面不至于空白），但水位按 0、
// 示例留空并给出原因。真实取号此时走的是模板级旧格式，与系统级规则无关，展示一个
// 按默认规则拼出来的“下一号”会与实际签发完全不一致，属于假数据。
//
// 规则行内容不合法或水位已耗尽时同样留空示例并给出原因，读取本身不失败，
// 管理员仍可通过 PUT 修正规则。
// 参数：ctx 为规则与编号登记查询的请求上下文。
// 返回值：实际规则视图；依赖故障返回错误，无法推算时不伪造示例。
func (s *Service) contractNumberRuleView(ctx context.Context) (ContractNumberRuleView, error) {
	store, err := s.contractNumberRuleStore()
	if err != nil {
		return ContractNumberRuleView{}, err
	}
	rule, found, err := store.GetContractNumberRule(ctx)
	if err != nil {
		return ContractNumberRuleView{}, err
	}
	// 展示用的 seq_year/current_value/sample 必须与真实取号同口径：取号按北京日历日取年，
	// 这里用 UTC 取年会在北京时间 1 月 1 日 00:00–08:00 把界面和示例单号算到上一年。
	year := businessclock.YearOf(time.Now())
	if !found {
		rule = contract.DefaultContractNumberRule()
		return ContractNumberRuleView{
			Prefix: rule.Prefix, YearSegment: rule.YearSegment, SeqWidth: rule.SeqWidth, Suffix: rule.Suffix,
			SeqYear: year, CurrentValue: 0, UpdatedBy: rule.UpdatedBy, UpdatedAt: rule.UpdatedAt,
			SampleUnavailableReason: "编号规则未初始化（迁移 000037 未应用或规则行缺失），" +
				"系统当前按模板级编号格式签发，无法预估下一个系统编号",
		}, nil
	}
	var current uint64
	ruleKey, keyErr := contract.ContractNumberRuleKey(rule)
	if keyErr == nil {
		current, err = store.ContractNumberSequenceValue(ctx, ruleKey, year)
		if err != nil {
			return ContractNumberRuleView{}, err
		}
	}
	view := ContractNumberRuleView{
		Prefix: rule.Prefix, YearSegment: rule.YearSegment, SeqWidth: rule.SeqWidth, Suffix: rule.Suffix,
		SeqYear: year, CurrentValue: current, UpdatedBy: rule.UpdatedBy, UpdatedAt: rule.UpdatedAt,
	}
	if keyErr != nil {
		view.SampleUnavailableReason = "编号规则内容不合法，无法推算下一个系统编号：" + contractNumberRuleReason(keyErr)
		return view, nil
	}
	// 示例单号只做拼装推演；水位耗尽时留空，并告知原因而不是展示一个假号。
	sample, buildErr := contract.BuildContractNumber(rule, year, current+1)
	if buildErr == nil {
		view.Sample = sample
		return view, nil
	}
	view.SampleUnavailableReason = "无法推算下一个系统编号：" + contractNumberRuleReason(buildErr)
	return view, nil
}

// contractNumberRuleStore 返回编号规则仓储；未配置时拒绝而不是伪造默认配置。
func (s *Service) contractNumberRuleStore() (ContractNumberRuleStore, error) {
	if s.Repo == nil {
		return nil, fmt.Errorf("contract number rule store is not configured")
	}
	store, ok := s.Repo.(ContractNumberRuleStore)
	if !ok || store == nil {
		return nil, fmt.Errorf("contract number rule store is not configured")
	}
	return store, nil
}

// contractNumberRuleReason 提取领域校验的中文原因，避免把包装前缀暴露给用户。
// 依次剥离编号规则相关的哨兵错误前缀（格式非法、流水耗尽、同号冲突），其余错误原样返回。
func contractNumberRuleReason(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.TrimSpace(err.Error())
	for _, sentinel := range []error{contract.ErrInvalidNumberRule, contract.ErrNumberExhausted, contract.ErrNumberConflict} {
		reason = strings.TrimSpace(strings.TrimPrefix(reason, sentinel.Error()+":"))
	}
	if reason == "" {
		return "合同编号规则不合法"
	}
	return reason
}
