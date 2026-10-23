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

// ProjectServiceItems 将涉及多个系统的合同服务展开为
// 稳定且可独立选择的项目服务项。同一投影
// 用于自动合同交付及人工建项
// 选择器，保证两条路径保留相同源标识及默认值。
func ProjectServiceItems(contractID string, items []ServiceItem) []ProjectServiceItem {
	result := make([]ProjectServiceItem, 0, len(items))
	for itemIndex, item := range items {
		systems := item.Systems
		if len(systems) == 0 {
			systems = []SystemInfo{{}}
		}
		for systemIndex, system := range systems {
			sourceID := strings.TrimSpace(item.SourceID)
			if sourceID == "" {
				sourceID = fmt.Sprintf("%s-%02d", strings.TrimSpace(contractID), itemIndex+1)
			}
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
