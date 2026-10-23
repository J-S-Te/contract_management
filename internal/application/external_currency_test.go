package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestExternalCurrencyUsesSameCodeRulesWithOrWithoutPaymentTerms(t *testing.T) {
	for _, currency := range []string{"CNY", " usd ", "123", "C1Y", "€", "CN", "CNYY"} {
		for _, scheduled := range []bool{false, true} {
			t.Run(currency+map[bool]string{true: "/scheduled", false: "/unscheduled"}[scheduled], func(t *testing.T) {
				repository := &recordingRepository{}
				service := &Service{Repo: repository, DetectionCategories: enabledDetectionCategories("软件测试"), CRMContractReferences: validCRMReference(7)}
				actor := Principal{TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create")}
				candidate := contract.Contract{Title: "外部合同", Type: "直签", CRMCustomerID: 7, CustomerName: "客户", AmountMinor: 10000, Currency: currency,
					Document: applicationTestDOCX(t, "外部合同正文"), ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "北京", Batch: "第一批次", Category: "软件测试", TestMode: "STANDARD"}}}
				if scheduled {
					candidate.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "100.00"}}
				}
				created, err := service.CreateExternalContract(context.Background(), actor, candidate)
				valid := currency == "CNY" || currency == " usd "
				if valid {
					if err != nil || created.Currency != "CNY" && created.Currency != "USD" {
						t.Fatalf("valid currency rejected: %v", err)
					}
				} else if !errors.Is(err, ErrValidation) || repository.created.ID != "" {
					t.Fatalf("invalid currency was persisted: %v", err)
				}
			})
		}
	}
}
