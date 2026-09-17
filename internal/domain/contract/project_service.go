package contract

import (
	"fmt"
	"strings"
)

// ProjectServiceItem is the smallest approved-contract projection required by
// Project Management. It intentionally excludes contract content, pricing,
// contacts and document bytes.
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

// ProjectServiceItems expands a contract service with multiple systems into
// stable, individually selectable project service items. The same projection
// is used by automatic contract delivery and the manual project-creation
// picker, so both paths preserve identical source identifiers and defaults.
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
