package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

// 合同总额必须按币种隔离，CNY 与 USD 不得相加成单一数值。
func TestDashboardCurrencyTotalsAreIsolated(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	for index, item := range []struct {
		currency string
		amount   int64
	}{{"CNY", 10000}, {"USD", 20000}, {"CNY", 5000}} {
		c := contract.Contract{
			ID: ulid.Make().String(), TenantID: tenant, Title: "币种测试", Type: "直签",
			ServiceType: "软件测试", OwnerUserID: "owner-1", OwnerIdentityID: "owner-1",
			AmountMinor: item.amount, Currency: item.currency, Content: "body", ContentHash: draftE2EHash("body"),
			Status: contract.StatusDraft, Version: 1,
		}
		if err := repository.CreateContract(ctx, c, "owner-1"); err != nil {
			t.Fatalf("create contract %d: %v", index, err)
		}
	}
	dashboard, err := repository.ContractDashboardScoped(ctx, contract.ScopeFilter{TenantID: tenant, AllowAll: true}, time.Now().UTC(), 50)
	if err != nil {
		t.Fatal(err)
	}
	byCurrency := map[string]contract.CurrencyTotal{}
	for _, item := range dashboard.CurrencyTotals {
		byCurrency[item.Currency] = item
	}
	if byCurrency["CNY"].AmountMinor != 15000 || byCurrency["CNY"].Contracts != 2 {
		t.Fatalf("CNY total = %+v", byCurrency["CNY"])
	}
	if byCurrency["USD"].AmountMinor != 20000 || byCurrency["USD"].Contracts != 1 {
		t.Fatalf("USD total = %+v", byCurrency["USD"])
	}
	if dashboard.TotalContracts != 3 || len(dashboard.CurrencyTotals) != 2 {
		t.Fatalf("dashboard = %+v", dashboard)
	}
}
