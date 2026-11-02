package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// contractNumberRuleMemoryStore 是编号规则仓储的内存实现，同时用于
// 验证仓储未实现该能力时应用层必须显式失败。
// current 表示编号规则指纹对应桶的全系统已用流水（不区分租户）。
// registered 是“全系统登记表里已有编号”的集合，键为小写形式，
// 用于模拟登记表主键大小写不敏感的占用判定。
type contractNumberRuleMemoryStore struct {
	Repository
	rule            contract.ContractNumberRule
	found           bool
	current         uint64
	registered      map[string]bool
	saved           []contract.ContractNumberRule
	saveError       error
	registryQueried []string
}

func (s *contractNumberRuleMemoryStore) GetContractNumberRule(context.Context) (contract.ContractNumberRule, bool, error) {
	return s.rule, s.found, nil
}

func (s *contractNumberRuleMemoryStore) SaveContractNumberRule(_ context.Context, rule contract.ContractNumberRule) error {
	if s.saveError != nil {
		return s.saveError
	}
	s.rule, s.found = rule, true
	s.saved = append(s.saved, rule)
	return nil
}

func (s *contractNumberRuleMemoryStore) ContractNumberSequenceValue(context.Context, string, int) (uint64, error) {
	return s.current, nil
}

// ContractNumberRegistryContains 模拟登记表主键的大小写不敏感占用判定。
func (s *contractNumberRuleMemoryStore) ContractNumberRegistryContains(_ context.Context, number string) (bool, error) {
	s.registryQueried = append(s.registryQueried, number)
	return s.registered[strings.ToLower(number)], nil
}

// contractNumberRuleStoreWithoutLookup 只实现规则读写，不实现登记表干跑检查，
// 用于验证既有 PUT 契约不受新增只读能力影响（不会因为缺少该能力而失败）。
type contractNumberRuleStoreWithoutLookup struct {
	Repository
	saved []contract.ContractNumberRule
}

func (s *contractNumberRuleStoreWithoutLookup) GetContractNumberRule(context.Context) (contract.ContractNumberRule, bool, error) {
	return contract.ContractNumberRule{}, false, nil
}

func (s *contractNumberRuleStoreWithoutLookup) SaveContractNumberRule(_ context.Context, rule contract.ContractNumberRule) error {
	s.saved = append(s.saved, rule)
	return nil
}

func (s *contractNumberRuleStoreWithoutLookup) ContractNumberSequenceValue(context.Context, string, int) (uint64, error) {
	return 0, nil
}

// plainContractRepository 只满足应用层 Repository，未实现编号规则能力。
type plainContractRepository struct{ Repository }

func contractNumberRuleAdmin() Principal {
	return Principal{TenantID: "tenant-1", UserID: "operator", Roles: []string{"admin"}}
}

// contractNumberRuleManager 是持有编号规则专用高权权限码的主体。
// 编号规则写入不提供合同 admin 角色旁路，因此所有写用例必须显式持有该权限码。
func contractNumberRuleManager() Principal {
	return Principal{TenantID: "tenant-1", UserID: "operator", Permissions: map[string]bool{"contract.number_rule.manage": true}}
}

func contractNumberRuleReader() Principal {
	return Principal{TenantID: "tenant-1", UserID: "reader", Permissions: map[string]bool{"contract.read": true}}
}

// TestGetContractNumberRulePermissions 验证读取权限与管理员旁路。
func TestGetContractNumberRulePermissions(t *testing.T) {
	store := &contractNumberRuleMemoryStore{rule: contract.DefaultContractNumberRule(), found: true}
	service := &Service{Repo: store}
	if _, err := service.GetContractNumberRule(context.Background(), Principal{TenantID: "tenant-1", UserID: "nobody"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("无 contract.read 应拒绝: %v", err)
	}
	if view, err := service.GetContractNumberRule(context.Background(), contractNumberRuleReader()); err != nil || view.Prefix != "HT-" {
		t.Fatalf("有 contract.read 应可读: view=%+v err=%v", view, err)
	}
	if _, err := service.GetContractNumberRule(context.Background(), contractNumberRuleAdmin()); err != nil {
		t.Fatalf("管理员应可读: %v", err)
	}
}

// TestGetContractNumberRuleSampleDoesNotConsume 验证示例单号只推演、不消耗流水。
func TestGetContractNumberRuleSampleDoesNotConsume(t *testing.T) {
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	store := &contractNumberRuleMemoryStore{
		rule:    contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4},
		found:   true,
		current: 7,
	}
	view, err := (&Service{Repo: store}).GetContractNumberRule(context.Background(), contractNumberRuleReader())
	if err != nil {
		t.Fatal(err)
	}
	wantSample := fmt.Sprintf("GTSR-%02d0008", year%100)
	if view.Sample != wantSample || view.CurrentValue != 7 || view.SeqYear != year ||
		view.Prefix != "GTSR-" || view.YearSegment != contract.NumberYearSegmentYY || view.SeqWidth != 4 {
		t.Fatalf("view=%+v want sample=%s", view, wantSample)
	}
	if len(store.saved) != 0 {
		t.Fatal("读取不得写入规则或消耗流水")
	}
}

// TestGetContractNumberRuleFallsBackToDefault 验证规则行缺失时：
// 规则字段仍展示迁移种子默认值，但示例单号必须留空并给出原因。
//
// 真实取号此时走模板级旧格式（与系统级规则无关），展示一个按默认规则拼出来的
// “下一号”与实际签发完全不一致，属于假数据；水位同理不得汇报成某个桶的值。
func TestGetContractNumberRuleFallsBackToDefault(t *testing.T) {
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	// current 故意给非零：规则行缺失时不得汇报任何桶的水位。
	view, err := (&Service{Repo: &contractNumberRuleMemoryStore{current: 5}}).
		GetContractNumberRule(context.Background(), contractNumberRuleAdmin())
	if err != nil {
		t.Fatal(err)
	}
	if view.Prefix != "HT-" || view.YearSegment != contract.NumberYearSegmentYYYY || view.SeqWidth != 4 ||
		view.CurrentValue != 0 || view.SeqYear != year {
		t.Fatalf("view=%+v", view)
	}
	if view.Sample != "" {
		t.Fatalf("规则行缺失时不得给出示例单号: %q", view.Sample)
	}
	if !strings.Contains(view.SampleUnavailableReason, "编号规则未初始化") {
		t.Fatalf("必须说明示例单号不可用的原因: %q", view.SampleUnavailableReason)
	}
}

// TestGetContractNumberRuleSampleEmptyWhenExhausted 验证水位耗尽时不给误导性的示例，
// 并给出可执行的原因说明。
func TestGetContractNumberRuleSampleEmptyWhenExhausted(t *testing.T) {
	store := &contractNumberRuleMemoryStore{
		rule:  contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 1},
		found: true, current: 9,
	}
	view, err := (&Service{Repo: store}).GetContractNumberRule(context.Background(), contractNumberRuleAdmin())
	if err != nil {
		t.Fatal(err)
	}
	if view.Sample != "" || view.CurrentValue != 9 {
		t.Fatalf("view=%+v", view)
	}
	if !strings.Contains(view.SampleUnavailableReason, "流水") {
		t.Fatalf("必须说明示例单号不可用的原因: %q", view.SampleUnavailableReason)
	}
}

// TestUpdateContractNumberRuleRejectsCaseOnlyPrefixChange 覆盖“只改前缀大小写”的场景：
// 规则指纹（字节级 sha256）对大小写敏感，改 GTSR- 为 gtsr- 会换到新流水桶从 1 开始，
// 但候选号在登记表主键（大小写不敏感）上等于历史号，保存后 50 次顺延即全系统停发。
// 因此保存前必须干跑一次登记表查重并拒绝，错误是 422 可修正配置（ErrValidation）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateContractNumberRuleRejectsCaseOnlyPrefixChange(t *testing.T) {
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	occupied := fmt.Sprintf("GTSR-%02d0001", year%100)
	store := &contractNumberRuleMemoryStore{registered: map[string]bool{strings.ToLower(occupied): true}}
	_, err := (&Service{Repo: store}).UpdateContractNumberRule(context.Background(), contractNumberRuleManager(),
		contract.ContractNumberRule{Prefix: "gtsr-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v want ErrValidation", err)
	}
	for _, want := range []string{"已被全系统登记表占用", "contract-number-audit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v want contains %q", err, want)
		}
	}
	if len(store.saved) != 0 {
		t.Fatal("命中历史号时不得写入规则")
	}
	// 干跑只查一次，且查的是“按新规则拼出的候选号”（即新规则桶的第一个号）。
	candidate := strings.ToLower(occupied)
	if len(store.registryQueried) != 1 || store.registryQueried[0] != candidate {
		t.Fatalf("干跑必须只查一次新规则的第一个候选号: %v", store.registryQueried)
	}
}

// TestUpdateContractNumberRuleAllowsFreshPrefix 验证候选号未被占用时正常保存。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateContractNumberRuleAllowsFreshPrefix(t *testing.T) {
	store := &contractNumberRuleMemoryStore{registered: map[string]bool{}}
	view, err := (&Service{Repo: store}).UpdateContractNumberRule(context.Background(), contractNumberRuleManager(),
		contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4})
	if err != nil {
		t.Fatalf("未被占用的规则必须可以保存: %v", err)
	}
	if len(store.saved) != 1 || store.saved[0].Prefix != "GTSR-" || view.Sample == "" {
		t.Fatalf("saved=%+v view=%+v", store.saved, view)
	}
}

// TestUpdateContractNumberRuleSkipsLookupWhenUnsupported 验证仓储未实现干跑能力时
// 保存流程保持原样（既有 PUT 契约不变）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateContractNumberRuleSkipsLookupWhenUnsupported(t *testing.T) {
	store := &contractNumberRuleStoreWithoutLookup{}
	if _, err := (&Service{Repo: store}).UpdateContractNumberRule(context.Background(), contractNumberRuleManager(),
		contract.DefaultContractNumberRule()); err != nil {
		t.Fatalf("缺少干跑能力时不得拒绝保存: %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved=%+v", store.saved)
	}
}

// TestUpdateContractNumberRuleNormalizesAndPersists 验证全量覆盖、归一化与审计字段。
func TestUpdateContractNumberRuleNormalizesAndPersists(t *testing.T) {
	store := &contractNumberRuleMemoryStore{current: 3}
	service := &Service{Repo: store}
	view, err := service.UpdateContractNumberRule(context.Background(), contractNumberRuleManager(), contract.ContractNumberRule{
		Prefix: " GTSR- ", YearSegment: "yy", SeqWidth: 4, Suffix: " ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved=%+v", store.saved)
	}
	saved := store.saved[0]
	if saved.Prefix != "GTSR-" || saved.YearSegment != contract.NumberYearSegmentYY || saved.SeqWidth != 4 ||
		saved.Suffix != "" || saved.UpdatedBy != "operator" || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved=%+v", saved)
	}
	if view.Sample == "" || !strings.HasPrefix(view.Sample, "GTSR-") || view.CurrentValue != 3 {
		t.Fatalf("view=%+v", view)
	}
}

// TestUpdateContractNumberRuleRejectsInvalidRules 验证校验失败返回中文原因且不落库。
func TestUpdateContractNumberRuleRejectsInvalidRules(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		rule   contract.ContractNumberRule
		reason string
	}{
		{name: "占位符前缀", rule: contract.ContractNumberRule{Prefix: "HT-{YYYY}", YearSegment: "YYYY", SeqWidth: 4}, reason: "只能包含字母、数字"},
		{name: "空白前缀", rule: contract.ContractNumberRule{Prefix: "HT -", YearSegment: "YYYY", SeqWidth: 4}, reason: "只能包含字母、数字"},
		{name: "中文前缀", rule: contract.ContractNumberRule{Prefix: "合同-", YearSegment: "YYYY", SeqWidth: 4}, reason: "只能包含字母、数字"},
		{name: "年份段未知", rule: contract.ContractNumberRule{Prefix: "HT-", YearSegment: "MM", SeqWidth: 4}, reason: "年份段"},
		{name: "位宽为零", rule: contract.ContractNumberRule{Prefix: "HT-", YearSegment: "YYYY", SeqWidth: 0}, reason: "流水位宽"},
		{name: "超长规则", rule: contract.ContractNumberRule{Prefix: strings.Repeat("P", 32), YearSegment: "YYYY", SeqWidth: 9, Suffix: strings.Repeat("S", 32)}, reason: "超过 64 个字符上限"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &contractNumberRuleMemoryStore{}
			_, err := (&Service{Repo: store}).UpdateContractNumberRule(context.Background(), contractNumberRuleManager(), testCase.rule)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err=%v want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Fatalf("err=%v want contains %q", err, testCase.reason)
			}
			if len(store.saved) != 0 {
				t.Fatal("校验失败不得写入规则")
			}
		})
	}
}

// TestUpdateContractNumberRulePermissions 验证写权限边界：
// 编号规则是全系统单行配置，只有专用权限码 contract.number_rule.manage 可写；
// contract.read、模板管理权限和合同 admin 角色都不构成写入许可（旁路已去掉）。
func TestUpdateContractNumberRulePermissions(t *testing.T) {
	store := &contractNumberRuleMemoryStore{}
	service := &Service{Repo: store}
	for _, testCase := range []struct {
		name   string
		actor  Principal
		reason string
	}{
		{
			name:   "只有读取权限",
			actor:  contractNumberRuleReader(),
			reason: "contract.read 不足以改全系统编号规则",
		},
		{
			name:   "只有模板管理权限",
			actor:  Principal{TenantID: "tenant-1", UserID: "template-manager", Permissions: map[string]bool{"contract.template.manage": true}},
			reason: "contract.template.manage 不再把守编号规则",
		},
		{
			name:   "合同管理员角色但无新权限码",
			actor:  Principal{TenantID: "tenant-1", UserID: "tenant-admin", Roles: []string{"admin"}},
			reason: "租户合同 admin 角色不得旁路跨租户配置",
		},
		{
			name: "管理员角色叠加模板管理权限仍无新码",
			actor: Principal{TenantID: "tenant-1", UserID: "tenant-admin", Roles: []string{"admin"},
				Permissions: map[string]bool{"contract.template.manage": true, "contract.read": true}},
			reason: "既有高权组合也不含新权限码",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := service.UpdateContractNumberRule(context.Background(), testCase.actor, contract.DefaultContractNumberRule()); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s: err=%v want ErrForbidden", testCase.reason, err)
			}
		})
	}
	if len(store.saved) != 0 {
		t.Fatalf("被拒绝的请求不得写入规则: %+v", store.saved)
	}
	// 只持有新权限码（没有任何其它合同权限、也没有 admin 角色）即可写入。
	if _, err := service.UpdateContractNumberRule(context.Background(), Principal{
		TenantID: "tenant-1", UserID: "number-rule-admin",
		Permissions: map[string]bool{"contract.number_rule.manage": true},
	}, contract.DefaultContractNumberRule()); err != nil {
		t.Fatalf("有 contract.number_rule.manage 应可写: %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("持有新权限码的请求必须写入规则: saved=%+v", store.saved)
	}
}

// TestContractNumberRuleStoreNotConfigured 验证仓储缺少能力时安全失败。
func TestContractNumberRuleStoreNotConfigured(t *testing.T) {
	service := &Service{Repo: &plainContractRepository{}}
	if _, err := service.GetContractNumberRule(context.Background(), contractNumberRuleAdmin()); err == nil {
		t.Fatal("未配置编号规则仓储时不得返回成功")
	}
	if _, err := service.UpdateContractNumberRule(context.Background(), contractNumberRuleManager(), contract.DefaultContractNumberRule()); err == nil {
		t.Fatal("未配置编号规则仓储时不得返回成功")
	}
	if _, err := (&Service{}).GetContractNumberRule(context.Background(), contractNumberRuleAdmin()); err == nil {
		t.Fatal("空仓储时不得返回成功")
	}
}
