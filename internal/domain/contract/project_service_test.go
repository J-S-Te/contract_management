package contract

import "testing"

func TestProjectServiceItemsUsesStableIdentifiersAndExpandsSystems(t *testing.T) {
	items := ProjectServiceItems("C-1", []ServiceItem{{
		ServiceType: "渗透测试",
		Systems:     []SystemInfo{{Name: "门户", Level: "三级"}, {Name: "管理端", Level: "二级"}},
	}})
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].SourceID != "C-1-01-01" || items[1].SourceID != "C-1-01-02" {
		t.Fatalf("source ids=%q,%q", items[0].SourceID, items[1].SourceID)
	}
	if items[0].Name != "渗透测试" || items[0].Category != "渗透测试" || items[0].TestMode != "PENETRATION" {
		t.Fatalf("first item=%+v", items[0])
	}
	if items[0].Site != "默认场所" || items[0].Batch != "默认批次" || items[0].SystemLevel != "三级" {
		t.Fatalf("defaults/system level=%+v", items[0])
	}
}
