package mysql

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestAssignedApprovalTaskStatuses(t *testing.T) {
	want := []approval.NodeStatus{approval.NodeActive, approval.NodePending}
	if got := assignedApprovalTaskStatuses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("assignedApprovalTaskStatuses() = %v, want %v", got, want)
	}
}

func TestContractFromRecordReadsNestedServiceItemsAndMapsLegacyRows(t *testing.T) {
	nested, err := json.Marshal([]contract.ServiceItem{{
		ServiceType: "等保测评",
		Systems:     []contract.SystemInfo{{Name: "业务系统", Level: "三级"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := contractFromRecord(contractRecord{ServiceType: "等保测评", ServiceItemsJSON: nested})
	if len(got.ServiceItems) != 1 || len(got.ServiceItems[0].Systems) != 1 {
		t.Fatalf("nested contract = %#v", got)
	}

	legacySystems, err := json.Marshal([]contract.SystemInfo{{Name: "旧系统", Level: "二级"}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := contractFromRecord(contractRecord{ServiceType: "软件测试", SystemsJSON: legacySystems})
	if len(legacy.ServiceItems) != 1 || legacy.ServiceItems[0].ServiceType != "软件测试" || len(legacy.ServiceItems[0].Systems) != 1 {
		t.Fatalf("legacy contract = %#v", legacy)
	}
}

func TestProjectDeliveryEligibleStatusesIncludeArchived(t *testing.T) {
	for _, status := range projectDeliveryEligibleStatuses() {
		if status == contract.StatusArchived {
			return
		}
	}
	t.Fatal("project delivery reconciliation must include archived contracts")
}
