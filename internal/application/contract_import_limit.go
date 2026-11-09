package application

import "fmt"

type ImportBatchItemLimitError struct {
	Limit int
	Count int
}

// Error 返回明确的批次数量限制，避免用户误改合法合同字段。
// 参数：无；读取本次清单数量与服务端配置上限。
// 返回值：可展示的中文数量提示。
func (e *ImportBatchItemLimitError) Error() string {
	return fmt.Sprintf("本批有 %d 份合同，当前每批最多 %d 份，请拆分批次后再预检。", e.Count, e.Limit)
}

// Unwrap 保留参数校验错误的兼容身份。
// 参数：无。
// 返回值：ErrValidation，供既有调用方分类。
func (e *ImportBatchItemLimitError) Unwrap() error { return ErrValidation }

// ImportBatchItemLimit 返回清单写入和前端能力响应共用的真实上限。
// 参数：无；读取当前服务的批次配置。
// 返回值：配置值；未配置时使用既有默认值100。
func (s *Service) ImportBatchItemLimit() int {
	if s == nil || s.ContractImportMaxItems == 0 {
		return 100
	}
	return s.ContractImportMaxItems
}
