package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

type reviewedIntakeRepository struct {
	*intakeOnlyRepository
	status string
}

// AcceptOpportunityIntake 模拟同事件已落库且已被人工审核的真实幂等返回。
// 参数：ctx为请求上下文，in为同一原始转交事件。
// 返回值：原接入ID和当前业务状态；不产生新接入记录。
func (r *reviewedIntakeRepository) AcceptOpportunityIntake(_ context.Context, in application.OpportunityIntake) (application.OpportunityIntake, error) {
	in.IntakeID = "intake-7"
	in.Status = r.status
	return in, nil
}

// TestOpportunityIntakeReviewedReplayKeepsTransportACK 验证人工审核不改变机器已受理回执。
// 参数：t为测试上下文，可将真实Handler生成的合成响应导出给CRM客户端。
// 返回值：无；业务终态改变传输ACK或回执身份时失败。
func TestOpportunityIntakeReviewedReplayKeepsTransportACK(t *testing.T) {
	var wires []json.RawMessage
	for _, status := range []string{application.OpportunityIntakeAccepted, application.OpportunityIntakeLinkConfirmed, application.OpportunityIntakeLinkException} {
		t.Run(status, func(t *testing.T) {
			repo := &reviewedIntakeRepository{intakeOnlyRepository: &intakeOnlyRepository{}, status: status}
			router := newOpportunityIntakeRouter(&application.Service{Repo: repo}, "tenant-a")
			response := postOpportunityIntake(router, `{"tenant_id":"tenant-a","event_id":"event-7","opportunity_id":7,"event_version":7,"customer_id":3,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-11-08T00:00:00Z"}`)
			wires = append(wires, append(json.RawMessage(nil), response.Body.Bytes()...))
			var envelope struct {
				Data struct {
					IntakeID       string `json:"intake_id"`
					EventID        string `json:"event_id"`
					Status         string `json:"status"`
					BusinessStatus string `json:"business_status"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusAccepted || envelope.Data.IntakeID != "intake-7" || envelope.Data.EventID != "event-7" || envelope.Data.Status != "ACCEPTED" || envelope.Data.BusinessStatus != status {
				t.Fatalf("已受理重放回执不稳定: http=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if path := os.Getenv("CRM_INTAKE_ACK_CONTRACT_FILE"); path != "" {
		raw, err := json.Marshal(wires)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
