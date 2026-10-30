package mysql

import "gorm.io/gorm"

// ordinaryContractScope 对普通派生路径应用统一来源策略。
// 参数：query为合同查询，alias为代码内固定表别名，不接受用户输入。
// 返回值：排除历史和未知来源的查询。
func ordinaryContractScope(query *gorm.DB, alias string) *gorm.DB {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return query.Where("COALESCE(NULLIF(" + prefix + "origin_kind,''),'NORMAL')='NORMAL'").
		Where("COALESCE(" + prefix + "import_mode,'NORMAL') IN ('','NORMAL')").Where(prefix + "import_policy_json IS NULL")
}

// ordinaryContractRecord 判定内存记录是否允许普通v1派生。
// 参数：row为权威合同行。
// 返回值：未知来源及历史策略失败关闭。
func ordinaryContractRecord(row contractRecord) bool {
	return (row.OriginKind == "" || row.OriginKind == "NORMAL") && (row.ImportMode == "" || row.ImportMode == "NORMAL") && len(row.ImportPolicyJSON) == 0
}
