package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// signingShipmentActor 构造有寄送登记能力的测试主体。
// 参数：无。
// 返回值：同租户测试主体及其签署权限。
func signingShipmentActor() Principal {
	return Principal{
		TenantID: "tenant-1", UserID: "sales-1", IdentityID: "sales-1", Roles: []string{"sales"},
		Permissions:      map[string]bool{"contract.signing.manage": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.signing.manage": {AllowAll: true}},
	}
}

// TestSaveSigningShipmentBlocksUploadedOrCompletedLedger 覆盖 M-7 应用层语义：已上传盖章件（
// pending_review）或已完成签署（completed）时登记寄出返回 409，且不调用仓储写入；仍可寄出的台账状
// 态不被误拦。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestSaveSigningShipmentBlocksUploadedOrCompletedLedger(t *testing.T) {
	approved := contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "sales-1", OwnerIdentityID: "sales-1", Status: contract.StatusActive, Version: 1}
	shipment := contract.SigningShipment{CourierNumber: "SF123456", RecipientName: "张三", RecipientPhone: "13800138000", RecipientAddress: "北京市朝阳区"}
	for _, status := range []contract.SigningStatus{contract.SigningPendingReview, contract.SigningCompleted} {
		t.Run(string(status), func(t *testing.T) {
			repo := &recordingRepository{contract: approved, signingRecord: contract.SigningRecord{Status: status}}
			svc := &Service{Repo: repo}
			err := svc.SaveSigningShipment(context.Background(), signingShipmentActor(), "contract-1", shipment)
			if !errors.Is(err, apperrors.ErrStateConflict) {
				t.Fatalf("status %s: err=%v, want ErrStateConflict", status, err)
			}
			if repo.signingShipmentWrites != 0 {
				t.Fatalf("status %s: repository write called %d times, want 0", status, repo.signingShipmentWrites)
			}
		})
	}
	for _, status := range []contract.SigningStatus{contract.SigningPendingShipment, contract.SigningInReturn} {
		t.Run(string(status)+"/allowed", func(t *testing.T) {
			repo := &recordingRepository{contract: approved, signingRecord: contract.SigningRecord{Status: status}}
			svc := &Service{Repo: repo}
			if err := svc.SaveSigningShipment(context.Background(), signingShipmentActor(), "contract-1", shipment); err != nil {
				t.Fatalf("status %s: err=%v, want nil", status, err)
			}
			if repo.signingShipmentWrites != 1 {
				t.Fatalf("status %s: repository writes=%d, want 1", status, repo.signingShipmentWrites)
			}
		})
	}
}
