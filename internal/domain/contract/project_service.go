package contract

import (
	"fmt"
	"strings"
)

// ProjectServiceItem 是项目管理所需的最小已审批合同
// 投影，有意排除合同正文、价格、
// 联系人及文档字节。
type ProjectServiceItem struct {
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	ServiceType string `json:"service_type"`
	Site        string `json:"site"`
	Batch       string `json:"batch"`
	Category    string `json:"category"`
	System      string `json:"system"`
	SystemLevel string `json:"system_level"`
	Requirement string `json:"requirement"`
	TestMode    string `json:"test_mode"`
}

// ProjectServiceItems 将合同服务按既有稳定身份展开为项目最小投影。
// 参数：contractID为权威合同身份，items为保持原顺序的合同服务事实。
// 返回值：不含价格和客户信息的项目服务；多系统继续追加原系统位置后缀。
func ProjectServiceItems(contractID string, items []ServiceItem) []ProjectServiceItem {
	result := make([]ProjectServiceItem, 0, len(items))
	for itemIndex, item := range items {
		systems := item.Systems
		if len(systems) == 0 {
			systems = []SystemInfo{{}}
		}
		for systemIndex, system := range systems {
			sourceID := ProjectServiceBaseID(contractID, itemIndex, item)
			if len(systems) > 1 {
				sourceID = fmt.Sprintf("%s-%02d", sourceID, systemIndex+1)
			}
			mode := strings.ToUpper(strings.TrimSpace(item.TestMode))
			if mode == "" {
				mode = "STANDARD"
				if strings.Contains(item.ServiceType, "渗透") || strings.Contains(strings.ToLower(item.ServiceType), "penetration") {
					mode = "PENETRATION"
				}
			}
			result = append(result, ProjectServiceItem{
				SourceID: sourceID, Name: firstProjectValue(item.Name, item.ServiceType), ServiceType: strings.TrimSpace(item.ServiceType),
				Site: firstProjectValue(item.Site, "默认场所"), Batch: firstProjectValue(item.Batch, "默认批次"),
				Category: firstProjectValue(item.Category, item.ServiceType), System: strings.TrimSpace(system.Name),
				SystemLevel: strings.TrimSpace(system.Level), Requirement: strings.TrimSpace(item.Requirement), TestMode: mode,
			})
		}
	}
	return result
}

func firstProjectValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// ProjectServiceBaseID 返回原项目投影唯一权威的服务基础身份。
// 参数：contractID为合同身份，itemIndex为原服务零基位置，item为原服务事实。
// 返回值：已有非空来源身份，或原合同ID与一基位置组成的确定性身份；不含多系统后缀。
func ProjectServiceBaseID(contractID string, itemIndex int, item ServiceItem) string {
	if sourceID := strings.TrimSpace(item.SourceID); sourceID != "" {
		return sourceID
	}
	return fmt.Sprintf("%s-%02d", strings.TrimSpace(contractID), itemIndex+1)
}
