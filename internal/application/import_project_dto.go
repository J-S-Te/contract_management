package application

import "github.com/j-s-te/contract-management/internal/domain/contract"

// importProjectService 仅包含T08历史项目协议接收的字段，不复用普通v1投影扩展字段。
type importProjectService struct {
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	Site        string `json:"site"`
	Batch       string `json:"batch"`
	Category    string `json:"category"`
	System      string `json:"system"`
	SystemLevel string `json:"system_level"`
	Requirement string `json:"requirement"`
	TestMode    string `json:"test_mode"`
}

// importProjectServices 复用服务展开身份并投影到严格的历史协议白名单。
// 参数：contractID为冻结合同ID，items为已规范化的原服务项。
// 返回值：T08允许的最小服务DTO，保留稳定source_id及系统展开，不输出service_type。
func importProjectServices(contractID string, items []contract.ServiceItem) []importProjectService {
	expanded := contract.ProjectServiceItems(contractID, items)
	result := make([]importProjectService, 0, len(expanded))
	for _, item := range expanded {
		result = append(result, importProjectService{
			SourceID: item.SourceID, Name: item.Name, Site: item.Site, Batch: item.Batch,
			Category: item.Category, System: item.System, SystemLevel: item.SystemLevel,
			Requirement: item.Requirement, TestMode: item.TestMode,
		})
	}
	return result
}
