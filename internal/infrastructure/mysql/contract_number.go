package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/docx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// globalContractNumberRuleID 是 con_contract_number_rule 唯一一行配置的主键。
const globalContractNumberRuleID = 1

// contractNumberConflictAttempts 限定“生成的单号已被占用”时的顺延次数，
// 避免唯一键冲突退化为无限取号。
const contractNumberConflictAttempts = 50

// GetContractNumberRule 读取系统级编号规则；规则行缺失时 found=false。
// 参数：ctx 为上下文。
// 返回值：归一化前的规则行内容、是否存在及数据库错误。
func (r *Repository) GetContractNumberRule(ctx context.Context) (contract.ContractNumberRule, bool, error) {
	return readContractNumberRule(r.db.WithContext(ctx))
}

// SaveContractNumberRule 全量覆盖系统级编号规则；缺失时插入固定主键行。
// 参数：ctx 为上下文，rule 为调用方已校验并补齐审计字段的规则。
// 返回值：数据库错误。
func (r *Repository) SaveContractNumberRule(ctx context.Context, rule contract.ContractNumberRule) error {
	record := contractNumberRuleRecord{
		ID: globalContractNumberRuleID, Prefix: rule.Prefix, YearSegment: rule.YearSegment,
		SeqWidth: uint8(rule.SeqWidth), Suffix: rule.Suffix,
		UpdatedBy: rule.UpdatedBy, UpdatedAt: rule.UpdatedAt,
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"prefix", "year_segment", "seq_width", "suffix", "updated_by", "updated_at"}),
	}).Create(&record).Error
}

// ContractNumberSequenceValue 返回指定年度、指定规则桶的全系统已用流水值，未使用过为 0。
// 参数：ctx 为上下文，ruleKey 为规则指纹（con_contract.ContractNumberRuleKey），year 为自然年。
// 返回值：当前水位或数据库错误。
func (r *Repository) ContractNumberSequenceValue(ctx context.Context, ruleKey string, year int) (uint64, error) {
	return contractNumberSequenceValue(r.db.WithContext(ctx), ruleKey, year)
}

// readContractNumberRule 在给定会话或事务内读取规则行。
func readContractNumberRule(db *gorm.DB) (contract.ContractNumberRule, bool, error) {
	var record contractNumberRuleRecord
	err := db.Where("id = ?", globalContractNumberRuleID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.ContractNumberRule{}, false, nil
	}
	if err != nil {
		return contract.ContractNumberRule{}, false, err
	}
	return contract.ContractNumberRule{
		Prefix: record.Prefix, YearSegment: record.YearSegment, SeqWidth: int(record.SeqWidth),
		Suffix: record.Suffix, UpdatedBy: record.UpdatedBy, UpdatedAt: record.UpdatedAt,
	}, true, nil
}

// contractNumberSequenceValue 在给定会话或事务内读取指定年度、规则桶的全系统流水水位。
func contractNumberSequenceValue(db *gorm.DB, ruleKey string, year int) (uint64, error) {
	var current uint64
	err := db.Model(&contractNumberSequenceRecord{}).Select("current_value").
		Where("seq_year = ? AND rule_key = ?", year, ruleKey).Scan(&current).Error
	return current, err
}

// allocateContractNumberInTx 在审批事务内按系统级规则分配并预占合同单号。
//
// 参数：tx 为已持有合同行锁的既有事务；tenantID 与 contractID 写入登记表用于追溯（
// 唯一性判定不区分租户）；legacyFormat 为旧格式回退用的模板级格式串；
// now 决定年份段与流水年度。
// 返回值：已由登记表主键独占的可写入 con_contract 的唯一单号，或流水耗尽等业务错误；
// 调用方必须整体回滚，登记表行随回滚释放，不会烧号。
//
// 降级条件被严格限定为「数据库尚未应用 000036/000037」这一种情况：规则表或登记表不存在
// （MySQL 1146）时回退旧的模板级格式串，且降级本身打一次告警便于观测。规则表读取的其它
// 错误（超时、权限、语法、连接中断等）一律原样返回，绝不静默降级——静默降级会绕过登记表，
// 让单号唯一性闭环出现缺口，也违反不吞错原则。规则行存在但内容非法时同样显式失败。
//
// 跨租户不重号由 con_contract_number_registry 的主键强制：流水只负责给出候选号。
// 流水桶按 (年份, 规则指纹) 计算，不区分租户；规则任一字段变化都会换到新桶、当年从 1
// 开始，流水表丢失后也会从 1 重新开始——这两种情况都会撞上历史编号，此时登记表主键
// 冲突让取号继续顺延，而不会把别人用过的号再签发一次。
func allocateContractNumberInTx(tx *gorm.DB, tenantID, contractID, legacyFormat string, now time.Time) (string, error) {
	rule, found, err := readContractNumberRule(tx)
	if err != nil {
		if !isContractNumberRelationMissing(err) {
			return "", err
		}
		warnContractNumberLegacyFallback("合同编号规则表不存在（迁移 000036 未应用），本次审批回退模板级编号格式且不写全系统登记表",
			tenantID, contractID, err)
		return formatContractNumber(legacyFormat, contractID, now), nil
	}
	if !found {
		warnContractNumberLegacyFallback("合同编号规则行为空（迁移 000036 尚未写入种子行），本次审批回退模板级编号格式且不写全系统登记表",
			tenantID, contractID, nil)
		return formatContractNumber(legacyFormat, contractID, now), nil
	}
	ruleKey, err := contract.ContractNumberRuleKey(rule)
	if err != nil {
		return "", err
	}
	// 年份段与年度流水桶必须按北京日历日取年：调用方传入的 now 是 UTC 时刻，
	// 直接用 now.Year() 会让北京时间 1 月 1 日 00:00–08:00 签发的合同落到上一年的
	// 年份段和 seq_year 桶里（写错数据，不只是显示问题）。now 本身仍原样用于
	// updated_at/issued_at 等时间戳写入，不换算成北京时间落库。
	year := businessclock.YearOf(now)
	for attempt := 0; attempt < contractNumberConflictAttempts; attempt++ {
		sequence, err := nextContractNumberSequence(tx, ruleKey, year, now)
		if err != nil {
			return "", err
		}
		number, err := contract.BuildContractNumber(rule, year, sequence)
		if err != nil {
			return "", err
		}
		reserved, err := reserveContractNumberInTx(tx, number, tenantID, contractID, year, ruleKey, now)
		if err != nil {
			return "", err
		}
		if reserved {
			return number, nil
		}
	}
	return "", fmt.Errorf("%w: 连续 %d 次生成的合同单号均被全系统登记表占用，请调整流水位宽或更换前缀",
		contract.ErrNumberExhausted, contractNumberConflictAttempts)
}

// warnContractNumberLegacyFallback 记录一次“未应用编号迁移、回退旧格式”的告警。
//
// 该降级是唯一允许的降级分支（见 allocateContractNumberInTx），必须留痕：
// 它意味着本库的编号不写全系统登记表，唯一性只在租户内成立。
// 参数：message 为告警文案，tenantID/contractID 为当前签发的租户与合同，cause 为原始错误（可为 nil）。
// 返回值：无。
func warnContractNumberLegacyFallback(message, tenantID, contractID string, cause error) {
	attributes := []any{"tenant_id", tenantID, "contract_id", contractID}
	if cause != nil {
		attributes = append(attributes, "error", cause)
	}
	slog.Warn(message, attributes...)
}

// nextContractNumberSequence 原子取规则桶的下一个年度流水值。
//
// LAST_INSERT_ID(expr) 把本次原子增量绑定在当前数据库连接上：首次插入分支也显式
// 写 LAST_INSERT_ID(1)，否则 SELECT LAST_INSERT_ID() 会读到连接上残留的旧值。
// INSERT ... ON DUPLICATE KEY UPDATE 同时为 (年度, 规则指纹) 行加行锁，使全系统并发
// 审批在同一规则桶上串行化；候选号是否真的可用仍由登记表主键最终裁决。
func nextContractNumberSequence(tx *gorm.DB, ruleKey string, year int, now time.Time) (uint64, error) {
	result := tx.Exec(`INSERT INTO con_contract_number_sequence (seq_year, rule_key, current_value, updated_at)
		VALUES (?, ?, LAST_INSERT_ID(1), ?)
		ON DUPLICATE KEY UPDATE current_value = LAST_INSERT_ID(current_value + 1), updated_at = ?`,
		year, ruleKey, now, now)
	if result.Error != nil {
		return 0, result.Error
	}
	var current uint64
	if err := tx.Raw("SELECT LAST_INSERT_ID()").Scan(&current).Error; err != nil {
		return 0, err
	}
	if current == 0 {
		return 0, fmt.Errorf("%w: 未能读取年度流水水位，请重试", contract.ErrNumberExhausted)
	}
	return current, nil
}

// contractNumberDuplicateKey 是 MySQL 唯一键冲突（ER_DUP_ENTRY）的错误码。
const contractNumberDuplicateKey = 1062

// contractNumberRelationMissingCode 是 MySQL 表不存在（ER_NO_SUCH_TABLE）的错误码。
// 命中它说明本库还没应用 000036/000037，是唯一允许降级到旧编号格式的场景。
const contractNumberRelationMissingCode = 1146

// reserveContractNumberInTx 在事务内预占一个合同单号。
//
// 参数：tx 为取号事务；number 为候选单号；tenantID/contractID 为签发对象；
// year 与 ruleKey 记录候选号来自哪个规则桶，便于事后核对；now 为签发时刻。
// 返回值：true 表示预占成功；false 表示该号已被全系统占用，调用方应继续顺延；
// 其他数据库错误原样返回。
//
// 唯一性只靠登记表主键：写入成功即为全系统独占，同事务内其余写入失败会整体回滚，
// 预占行随之释放。重复键冲突在 MySQL/InnoDB 里只回滚当前语句、不废掉整个事务，
// 因此可以在同一事务里继续尝试下一个流水值。
//
// 1062 不再无条件当成“号被占”：命中后再回查登记表确认候选号确实已被登记，
// 否则原样返回原始错误。这样将来给登记表加第二个唯一键时，真实故障不会被
// 误判成“顺延 50 次后流水耗尽”而藏起来。
func reserveContractNumberInTx(tx *gorm.DB, number, tenantID, contractID string, year int, ruleKey string, now time.Time) (bool, error) {
	result := tx.Exec(`INSERT INTO con_contract_number_registry
		(contract_number, tenant_id, contract_id, seq_year, rule_key, issued_at)
		VALUES (?, ?, ?, ?, ?, ?)`, number, tenantID, contractID, year, ruleKey, now)
	if result.Error == nil {
		return true, nil
	}
	if !isContractNumberDuplicate(result.Error) {
		return false, result.Error
	}
	registered, err := contractNumberRegisteredInTx(tx, number)
	if err != nil {
		return false, err
	}
	if !registered {
		return false, result.Error
	}
	return false, nil
}

// contractNumberRegisteredInTx 确认某个候选号是否真的已被登记表占用。
//
// 必须用加锁读（FOR SHARE）而不是普通快照读：事务隔离级别是 REPEATABLE READ，
// 冲突行可能由并发事务在本事务快照建立之后才提交，普通 SELECT 看不到它，
// 那会把真实的“号被占”误判成其它唯一键故障，导致取号直接失败而不是顺延。
// 加锁读始终读到最新已提交版本，与刚才触发 1062 的那一行一致。
//
// 参数：tx 为取号事务，number 为候选单号。
// 返回值：是否已登记，或数据库错误。
func contractNumberRegisteredInTx(tx *gorm.DB, number string) (bool, error) {
	var found int
	row := tx.Raw(`SELECT 1 FROM con_contract_number_registry WHERE contract_number = ? LIMIT 1 FOR SHARE`, number).Row()
	if err := row.Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return found == 1, nil
}

// ContractNumberRegistryContains 判断候选单号是否已被全系统登记表占用。
//
// 该查询用于保存规则前的干跑检查：登记表主键列是 utf8mb4_unicode_ci，
// 因此比较不区分大小写——把前缀从 GTSR- 改成 gtsr- 会换到新流水桶从 1 开始，
// 但候选号在登记表主键上仍然等于历史号，盲目保存会让全系统在 50 次顺延后停发。
//
// 参数：ctx 为上下文，number 为候选单号。
// 返回值：是否已占用；登记表不存在（尚未应用 000037）时按未占用处理并打一次告警。
func (r *Repository) ContractNumberRegistryContains(ctx context.Context, number string) (bool, error) {
	var found int
	err := r.db.WithContext(ctx).Raw(
		`SELECT 1 FROM con_contract_number_registry WHERE contract_number = ? LIMIT 1`, number).Row().Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if isContractNumberRelationMissing(err) {
			slog.Warn("合同编号登记表不存在（迁移 000037 未应用），跳过保存规则前的号码占用干跑检查",
				"contract_number", number, "error", err)
			return false, nil
		}
		return false, err
	}
	return found == 1, nil
}

// isContractNumberDuplicate 识别唯一键冲突（1062）。
//
// 本仓库既有写法是 gorm.ErrDuplicatedKey（bootstrap 打开 TranslateError）加驱动文本
// 兜底；这里以驱动错误码为主，同时接受 GORM 翻译结果，避免依赖错误文本。
func isContractNumberDuplicate(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == contractNumberDuplicateKey
	}
	return errors.Is(err, gorm.ErrDuplicatedKey)
}

// isContractNumberRelationMissing 识别「表不存在」（1146）。
//
// 生产构造仓储时打开了 GORM TranslateError，但 GORM 只翻译 1062/1451/1452，
// 1146 仍以 *mysql.MySQLError 原样返回，因此错误码判定是主路径；同时保留
// errors.Is(gorm.ErrRecordNotFound) 之外的文本兜底，避免连接池或驱动把错误
// 包装成别的类型后，把“本库还没迁移”错判成硬失败。
func isContractNumberRelationMissing(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == contractNumberRelationMissingCode
	}
	message := err.Error()
	return strings.Contains(message, "Error 1146") || strings.Contains(message, "doesn't exist")
}

// approvedNumberUpdates 仅为DOCX回写正文编号，PDF保持原字节与哈希。
// 参数：current为审批锁定的合同行，number为本次正式系统编号。
// 返回值：应更新的字段；未知类型或无效文档返回错误，事务不得完成。
func approvedNumberUpdates(current contractRecord, number string) (map[string]any, error) {
	updates := map[string]any{"contract_number": number}
	if len(current.RenderedDocument) == 0 {
		return updates, nil
	}
	format, err := contractdocument.EffectiveDocumentFormat(current.DocumentFormat, current.RenderedDocument)
	if err != nil {
		return nil, err
	}
	if format == contractdocument.FormatPDF {
		return updates, nil
	}
	document, err := docx.ReplaceSystemContractNumber(current.RenderedDocument, number)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(document, current.RenderedDocument) {
		return updates, nil
	}
	content, err := docx.PlainText(document)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(document)
	updates["rendered_document"], updates["content"], updates["content_hash"] = document, content, hex.EncodeToString(hash[:])
	if len(current.TemplateValuesJSON) > 0 {
		values := map[string]string{}
		if err := json.Unmarshal(current.TemplateValuesJSON, &values); err != nil {
			return nil, err
		}
		for _, key := range []string{"合同编号", "系统合同编号"} {
			if _, ok := values[key]; ok {
				values[key] = number
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		updates["template_values_json"] = encoded
	}
	return updates, nil
}
