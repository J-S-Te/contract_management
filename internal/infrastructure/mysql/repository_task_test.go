package mysql

import (
	"reflect"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
)

func TestAssignedApprovalTaskStatuses(t *testing.T) {
	want := []approval.NodeStatus{approval.NodeActive, approval.NodePending}
	if got := assignedApprovalTaskStatuses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("assignedApprovalTaskStatuses() = %v, want %v", got, want)
	}
}
