package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 本文件把「部署前人工跑两条 SQL 找重号」变成迁移执行前的自动预检，
// 并把同一套口径的巡检能力（AuditContractNumberRegistry）暴露给运维命令。
// 迁移入口 migration.Run 是唯一执行点，因此任何调用方都自动带上预检。

const (
	// contractTable 是合同主数据表，编号与租户的唯一事实来源。
	contractTable = "con_contract"
	// registryTable 是 000037 建立的全系统合同编号登记表。
	registryTable = "con_contract_number_registry"
	// preflightSampleLimit 限制迁移错误信息里逐条列出的重复编号组数。
	preflightSampleLimit = 20
	// auditSampleLimit 限制巡检每一类问题返回的明细条数，总数由报告另行给出。
	auditSampleLimit = 20
	// describeNumberOptions 限制单组重号展示的原文写法数量，避免错误信息过长。
	describeNumberOptions = 5
)

// DuplicateContractNumber 是一组重复的合同编号：精确相同，或按登记表主键排序规则
// （utf8mb4_unicode_ci，大小写与尾随空格视为相同）归一后相同。
type DuplicateContractNumber struct {
	// Normalized 是归一后的编号，也是去重口径与排序口径。
	Normalized string
	// Numbers 是归一后相同但原文不同的写法，已去重排序。
	Numbers []string
	// Count 是该组涉及的合同行数。
	Count int
	// Tenants 是该组涉及的租户，已去重排序。
	Tenants []string
}

// String 生成给运维看的中文描述，保留「编号 X 出现 C 次，涉及租户：T」的固定句式。
// 参数：无。
// 返回值：单组重号的中文描述。
func (d DuplicateContractNumber) String() string {
	label := d.Normalized
	extra := ""
	if len(d.Numbers) == 1 {
		label = d.Numbers[0]
		if label != d.Normalized {
			extra = "（归一后 " + d.Normalized + "）"
		}
	} else if len(d.Numbers) > 1 {
		shown := d.Numbers
		if len(shown) > describeNumberOptions {
			shown = shown[:describeNumberOptions]
		}
		extra = "（归一后相同写法：" + strings.Join(shown, "、") + "）"
	}
	tenants := strings.Join(d.Tenants, ",")
	if tenants == "" {
		tenants = "（未知）"
	}
	return fmt.Sprintf("编号 %s 出现 %d 次，涉及租户：%s%s", label, d.Count, tenants, extra)
}

// ContractNumberConflictError 表示预检发现 con_contract 存在重复合同编号。
//
// 错误正文是面向运维的中文报告：说明迁移已中止、未写入任何数据、逐条列出前 20 组
// 编号与租户，并给出处理指引。底层查询错误不会进正文，避免把裸 MySQL 报错抛给运维。
type ContractNumberConflictError struct {
	// Groups 是全部重复组，按归一后编号排序；报告只展示前 preflightSampleLimit 组。
	Groups []DuplicateContractNumber
}

// Error 生成面向运维的中文报告。
// 参数：无。
// 返回值：含组数、逐条编号与租户、剩余组数指引和处理指引的中文文本。
func (e *ContractNumberConflictError) Error() string {
	var report strings.Builder
	fmt.Fprintf(&report, "检测到 %d 组重复的合同编号，迁移已中止，未写入任何数据。", len(e.Groups))
	shown := e.Groups
	if len(shown) > preflightSampleLimit {
		shown = shown[:preflightSampleLimit]
	}
	for _, group := range shown {
		report.WriteString("\n  ")
		report.WriteString(group.String())
	}
	if rest := len(e.Groups) - len(shown); rest > 0 {
		fmt.Fprintf(&report, "\n……其余 %d 组请用 cmd/contract-number-audit 查看完整清单", rest)
	}
	report.WriteString("\n处理指引：请先清理或受控更正 con_contract.contract_number 中的重复编号")
	report.WriteString("（同一编号只能归属一个租户的一份合同），再重新执行迁移；")
	report.WriteString("本迁移的存量回填可安全重跑：已登记的编号会被自动跳过，不会因续跑报重复键。")
	return report.String()
}

// UnregisteredContractNumber 是一条写了编号却没有登记表记录的合同行。
// 常见成因：绕过取号流程直接写库，或历史存量未回填。
type UnregisteredContractNumber struct {
	// ContractNumber 是合同行上的编号原文。
	ContractNumber string
	// TenantID 是合同所属租户。
	TenantID string
	// ContractID 是合同主键。
	ContractID string
}

// String 生成给运维看的中文描述。
// 参数：无。
// 返回值：单条未登记记录的中文描述。
func (u UnregisteredContractNumber) String() string {
	return fmt.Sprintf("编号 %s 未登记，租户 %s，合同 %s", u.ContractNumber, u.TenantID, u.ContractID)
}

// RegistryMismatch 是一条登记表与合同行租户或合同 ID 不一致的记录。
type RegistryMismatch struct {
	// ContractNumber 是双方共用的编号。
	ContractNumber string
	// ContractTenantID 与 ContractID 来自合同行。
	ContractTenantID string
	ContractID       string
	// RegistryTenantID 与 RegistryContractID 来自登记表。
	RegistryTenantID   string
	RegistryContractID string
}

// String 生成给运维看的中文描述。
// 参数：无。
// 返回值：单条错配记录的中文描述。
func (m RegistryMismatch) String() string {
	return fmt.Sprintf("编号 %s 登记错配：合同行租户 %s / 合同 %s，登记表租户 %s / 合同 %s",
		m.ContractNumber, m.ContractTenantID, m.ContractID, m.RegistryTenantID, m.RegistryContractID)
}

// AuditReport 是合同编号与登记表一致性巡检结果。
// 重复编号给出全部组；未登记与错配按 auditSampleLimit 截断，总数单独记录。
type AuditReport struct {
	// Duplicates 是全部重复编号组，按归一后编号排序。
	Duplicates []DuplicateContractNumber
	// Unregistered 是最多 auditSampleLimit 条未登记明细。
	Unregistered []UnregisteredContractNumber
	// UnregisteredTotal 是未登记合同行总数。
	UnregisteredTotal int
	// Mismatched 是最多 auditSampleLimit 条登记错配明细。
	Mismatched []RegistryMismatch
	// MismatchedTotal 是登记错配合同行总数。
	MismatchedTotal int
}

// Clean 表示巡检未发现任何问题。
// 参数：无。
// 返回值：三类问题均为零时为 true。
func (r AuditReport) Clean() bool {
	return len(r.Duplicates) == 0 && r.UnregisteredTotal == 0 && r.MismatchedTotal == 0
}

// Total 返回三类问题的合计数量，用于结论行。
// 参数：无。
// 返回值：重复组数 + 未登记行数 + 错配行数。
func (r AuditReport) Total() int {
	return len(r.Duplicates) + r.UnregisteredTotal + r.MismatchedTotal
}

// PreflightContractNumbers 在应用 000037 之前检查 con_contract 是否存在重复编号。
//
// 放行判定只有一条：按 contract_number 原值 GROUP BY 是否出现重复组。该列排序规则是
// utf8mb4_unicode_ci，分组天然继承它，因此“大小写/尾随空格视为相同”与登记表主键
// 语义完全一致，不需要任何额外归一。
//
// 第二遍按 UPPER(TRIM(contract_number)) 分组只用于补充报告信息（展示“归一后相同的
// 多种写法”），不参与放行判定：它的语义与主键排序规则并不等价——unicode_ci 把重音
// 与全角字符也算作相同，而 UPPER(TRIM()) 不会；反过来 UPPER(TRIM()) 会把仅前导空格
// 不同的编号算成一组，而主键并不会。用第二遍判定既会漏报又会误报。
//
// con_contract 表不存在（全新库首次部署）时直接通过；无重号返回 nil；
// 有重号返回 *ContractNumberConflictError。
//
// 参数：ctx 为上下文，database 为迁移专用连接池。
// 返回值：无重号返回 nil，否则返回中文冲突报告或带 %w 原因的查询错误。
func PreflightContractNumbers(ctx context.Context, database *sql.DB) error {
	exists, err := tableExists(ctx, database, contractTable)
	if err != nil {
		return fmt.Errorf("合同编号预检无法读取库表信息，迁移已中止，请联系技术排查：%w", err)
	}
	if !exists {
		// 全新库尚未建 con_contract，没有存量编号可冲突。
		return nil
	}
	groups, err := duplicateContractNumbers(ctx, database)
	if err != nil {
		return fmt.Errorf("合同编号预检无法完成重号统计，迁移已中止，请联系技术排查：%w", err)
	}
	if len(groups) == 0 {
		return nil
	}
	return &ContractNumberConflictError{Groups: groups}
}

// AuditContractNumberRegistry 巡检合同编号与登记表的一致性。
//
// 输出三类问题：重复编号（与 PreflightContractNumbers 同口径）、未登记（合同行有编号
// 但登记表无对应行）、登记错配（登记表与合同行的租户或合同 ID 不一致）。
//
// 参数：ctx 为上下文，database 为巡检连接池（调用方应使用只读会话）。
// 返回值：巡检报告；con_contract 或登记表不存在时返回中文错误。
func AuditContractNumberRegistry(ctx context.Context, database *sql.DB) (AuditReport, error) {
	for _, required := range []struct{ table, hint string }{
		{contractTable, "合同主数据表 con_contract 不存在：当前库还不是合同服务库，请确认 MYSQL_DSN 指向正确的数据库。"},
		{registryTable, "合同编号登记表 con_contract_number_registry 不存在：当前库尚未部署迁移 000037，请先执行 cmd/migrate 后再巡检。"},
	} {
		exists, err := tableExists(ctx, database, required.table)
		if err != nil {
			return AuditReport{}, fmt.Errorf("巡检无法读取库表信息，请联系技术排查：%w", err)
		}
		if !exists {
			return AuditReport{}, errors.New(required.hint)
		}
	}
	duplicates, err := duplicateContractNumbers(ctx, database)
	if err != nil {
		return AuditReport{}, fmt.Errorf("巡检无法完成重号统计，请联系技术排查：%w", err)
	}
	unregisteredTotal, unregistered, err := auditUnregisteredContractNumbers(ctx, database)
	if err != nil {
		return AuditReport{}, err
	}
	mismatchedTotal, mismatched, err := auditRegistryMismatches(ctx, database)
	if err != nil {
		return AuditReport{}, err
	}
	return AuditReport{
		Duplicates:        duplicates,
		Unregistered:      unregistered,
		UnregisteredTotal: unregisteredTotal,
		Mismatched:        mismatched,
		MismatchedTotal:   mismatchedTotal,
	}, nil
}

// contractNumberFilter 限定「有编号」的合同行；prefix 为查询里的表别名或表名。
// NULL 与空串不参与唯一性裁决，与 000037 回填的 WHERE 完全一致。
func contractNumberFilter(prefix string) string {
	return prefix + ".contract_number IS NOT NULL AND " + prefix + ".contract_number <> ''"
}

// duplicateContractNumbers 统计重复编号：第一遍判定，第二遍仅补充报告信息。
//
// 放行判定只看第一遍（按 contract_number 原值分组）。第二遍按 UPPER(TRIM(...)) 分组，
// 只用来给已判定的重复组补充“归一后相同的多种写法”，绝不新增判定组：
// 它的比较语义与登记表主键排序规则不等价（漏重音/全角，多前导空格），
// 拿它放行会误报甚至挡住本来能迁移的库。
//
// 参数：ctx 为上下文，database 为连接池。
// 返回值：按归一后编号排序的重复组；第一遍无重复返回空切片（放行）。
func duplicateContractNumbers(ctx context.Context, database *sql.DB) ([]DuplicateContractNumber, error) {
	// 口径 1：按编号原值分组（列排序规则 utf8mb4_unicode_ci，忽略大小写与尾随空格）。
	// 这是唯一放行判定，与登记表主键的比较语义完全一致。
	exact, err := queryDuplicateNumbers(ctx, database, `SELECT contract_number,
	UPPER(TRIM(contract_number)) AS normalized,
	COUNT(*) AS occurrences,
	GROUP_CONCAT(DISTINCT tenant_id ORDER BY tenant_id SEPARATOR ',') AS tenants
FROM `+contractTable+`
WHERE `+contractNumberFilter(contractTable)+`
GROUP BY contract_number
HAVING occurrences > 1`, false)
	if err != nil {
		return nil, err
	}
	if len(exact) == 0 {
		return nil, nil
	}
	// 口径 2：仅用于报告补充“归一后相同的写法”，不参与放行判定。
	normalized, err := queryDuplicateNumbers(ctx, database, `SELECT UPPER(TRIM(contract_number)) AS normalized,
	COUNT(*) AS occurrences,
	GROUP_CONCAT(DISTINCT tenant_id ORDER BY tenant_id SEPARATOR ',') AS tenants,
	GROUP_CONCAT(DISTINCT contract_number ORDER BY contract_number SEPARATOR '|') AS numbers
FROM `+contractTable+`
WHERE `+contractNumberFilter(contractTable)+`
GROUP BY UPPER(TRIM(contract_number))
HAVING occurrences > 1`, true)
	if err != nil {
		return nil, err
	}
	return mergeDuplicateNumberReport(exact, normalized), nil
}

// mergeDuplicateNumberReport 把第一遍的判定结果与第二遍的报告信息合并。
//
// 判定集只可能被补充写法，不可能被扩增：第二遍里第一遍没有判定的分组直接丢弃，
// 保证“能不能放行”完全由第一遍决定。
//
// 参数：released 为第一遍判定出的重复组，informational 为第二遍的归一分组。
// 返回值：按归一后编号排序的重复组；released 为空时返回 nil。
func mergeDuplicateNumberReport(released, informational []DuplicateContractNumber) []DuplicateContractNumber {
	merged := make(map[string]*DuplicateContractNumber, len(released))
	for _, group := range released {
		copied := group
		merged[group.Normalized] = &copied
	}
	for _, group := range informational {
		existing, found := merged[group.Normalized]
		if !found {
			// 第二遍独有的组（例如仅前导空格不同）不参与放行判定，也不进报告。
			continue
		}
		if group.Count > existing.Count {
			existing.Count = group.Count
		}
		existing.Numbers = unionStrings(existing.Numbers, group.Numbers)
		existing.Tenants = unionStrings(existing.Tenants, group.Tenants)
	}
	groups := make([]DuplicateContractNumber, 0, len(merged))
	for _, group := range merged {
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].Normalized < groups[right].Normalized })
	return groups
}

// queryDuplicateNumbers 执行一条重号分组查询。
//
// 参数：ctx 为上下文，database 为连接池，query 为已含 GROUP BY 与 HAVING 的查询，
// withNumbers 表示结果是否带原始写法列（精确分组时编号列本身即写法）。
// 返回值：查询到的重复组；SQL 失败返回原始错误，由调用方包装成中文提示。
func queryDuplicateNumbers(ctx context.Context, database *sql.DB, query string, withNumbers bool) ([]DuplicateContractNumber, error) {
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]DuplicateContractNumber, 0, 8)
	for rows.Next() {
		var number, normalized, tenants sql.NullString
		var occurrences int
		var numbers sql.NullString
		if withNumbers {
			if err := rows.Scan(&normalized, &occurrences, &tenants, &numbers); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&number, &normalized, &occurrences, &tenants); err != nil {
				return nil, err
			}
			numbers = number
		}
		groups = append(groups, DuplicateContractNumber{
			Normalized: normalized.String,
			Numbers:    splitConcatenated(numbers.String, "|"),
			Count:      occurrences,
			Tenants:    splitConcatenated(tenants.String, ","),
		})
	}
	return groups, rows.Err()
}

// splitConcatenated 拆分 GROUP_CONCAT 结果并去掉空值。
//
// 参数：value 为 GROUP_CONCAT 文本，separator 为聚合时使用的分隔符。
// 返回值：按原文顺序的片段；空文本返回 nil。
func splitConcatenated(value, separator string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// unionStrings 返回两个字符串切片的去重并集并按字典序排序，保证报告稳定可复核。
//
// 参数：left、right 为待合并切片。
// 返回值：去重排序后的新切片；两者皆空返回 nil。
func unionStrings(left, right []string) []string {
	if len(right) == 0 {
		return left
	}
	seen := make(map[string]struct{}, len(left)+len(right))
	result := make([]string, 0, len(left)+len(right))
	for _, value := range append(append([]string{}, left...), right...) {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// auditUnregisteredContractNumbers 统计「有编号但登记表无对应行」的合同行。
//
// 参数：ctx 为上下文，database 为连接池。
// 返回值：问题总数、最多 auditSampleLimit 条明细及错误。
func auditUnregisteredContractNumbers(ctx context.Context, database *sql.DB) (int, []UnregisteredContractNumber, error) {
	condition := ` FROM ` + contractTable + ` AS c
LEFT JOIN ` + registryTable + ` AS r ON r.contract_number = c.contract_number
WHERE ` + contractNumberFilter("c") + ` AND r.contract_number IS NULL`
	total, err := countAuditRows(ctx, database, `SELECT COUNT(*)`+condition)
	if err != nil {
		return 0, nil, fmt.Errorf("巡检无法统计未登记编号，请联系技术排查：%w", err)
	}
	if total == 0 {
		return 0, nil, nil
	}
	rows, err := database.QueryContext(ctx,
		`SELECT c.contract_number, c.tenant_id, c.id`+condition+
			` ORDER BY c.contract_number, c.tenant_id LIMIT ?`, auditSampleLimit)
	if err != nil {
		return 0, nil, fmt.Errorf("巡检无法读取未登记编号明细，请联系技术排查：%w", err)
	}
	defer rows.Close()
	details := make([]UnregisteredContractNumber, 0, auditSampleLimit)
	for rows.Next() {
		var detail UnregisteredContractNumber
		if err := rows.Scan(&detail.ContractNumber, &detail.TenantID, &detail.ContractID); err != nil {
			return 0, nil, fmt.Errorf("巡检无法读取未登记编号明细，请联系技术排查：%w", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("巡检无法读取未登记编号明细，请联系技术排查：%w", err)
	}
	return total, details, nil
}

// auditRegistryMismatches 统计登记表与合同行租户或合同 ID 不一致的记录。
//
// 参数：ctx 为上下文，database 为连接池。
// 返回值：问题总数、最多 auditSampleLimit 条明细及错误。
func auditRegistryMismatches(ctx context.Context, database *sql.DB) (int, []RegistryMismatch, error) {
	// 租户与合同 ID 要求逐字节相等：两侧列字符集/排序规则不同（合同行 ascii_bin，
	// 登记表 utf8mb4_unicode_ci），显式 CAST 成二进制既避免排序规则混用报错，
	// 也让「大小写或空格不同」必然算作错配，而不是被大小写不敏感的比较放过。
	condition := ` FROM ` + contractTable + ` AS c
JOIN ` + registryTable + ` AS r ON r.contract_number = c.contract_number
WHERE ` + contractNumberFilter("c") + `
AND (CAST(r.tenant_id AS BINARY) <> CAST(c.tenant_id AS BINARY) OR CAST(r.contract_id AS BINARY) <> CAST(c.id AS BINARY))`
	total, err := countAuditRows(ctx, database, `SELECT COUNT(*)`+condition)
	if err != nil {
		return 0, nil, fmt.Errorf("巡检无法统计登记错配，请联系技术排查：%w", err)
	}
	if total == 0 {
		return 0, nil, nil
	}
	rows, err := database.QueryContext(ctx,
		`SELECT c.contract_number, c.tenant_id, c.id, r.tenant_id, r.contract_id`+condition+
			` ORDER BY c.contract_number, c.tenant_id LIMIT ?`, auditSampleLimit)
	if err != nil {
		return 0, nil, fmt.Errorf("巡检无法读取登记错配明细，请联系技术排查：%w", err)
	}
	defer rows.Close()
	details := make([]RegistryMismatch, 0, auditSampleLimit)
	for rows.Next() {
		var detail RegistryMismatch
		if err := rows.Scan(&detail.ContractNumber, &detail.ContractTenantID, &detail.ContractID,
			&detail.RegistryTenantID, &detail.RegistryContractID); err != nil {
			return 0, nil, fmt.Errorf("巡检无法读取登记错配明细，请联系技术排查：%w", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("巡检无法读取登记错配明细，请联系技术排查：%w", err)
	}
	return total, details, nil
}

// countAuditRows 执行巡检的计数查询。
//
// 参数：ctx 为上下文，database 为连接池，query 为 SELECT COUNT(*) 语句。
// 返回值：计数结果或原始错误。
func countAuditRows(ctx context.Context, database *sql.DB, query string) (int, error) {
	var total int
	if err := database.QueryRowContext(ctx, query).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// tableExists 判断当前 DATABASE() 下是否存在指定表。
//
// 参数：ctx 为上下文，database 为连接池，table 为表名。
// 返回值：是否存在及数据库错误。
func tableExists(ctx context.Context, database *sql.DB, table string) (bool, error) {
	var found int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&found); err != nil {
		return false, err
	}
	return found > 0, nil
}
