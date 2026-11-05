package mysql

import (
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// localOutboundNotApplicable 判定普通本地来源已冻结的不适用出站，防止后装自动回放。
// 参数：row为数据库权威合同，不使用请求声明的策略。
// 返回值：是否不适用；本地来源策略损坏时明确失败，绝不发送假CRM身份。
func localOutboundNotApplicable(row contractRecord) (bool, error) {
	local := row.CounterpartySource == contract.CounterpartyLocal || row.CategorySource == contract.CategoryLocal
	if !local {
		return false, nil
	}
	if row.IntegrationPolicy != contract.IntegrationNotApplicable {
		return false, application.ErrContractSourceConflict
	}
	return true, nil
}
