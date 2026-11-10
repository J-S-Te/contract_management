package mysql

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

type signedCountFixture struct {
	name      string
	origin    string
	mode      contract.ImportMode
	status    contract.Status
	confirmed bool
	signed    bool
	policy    bool
	want      uint64
}

// seedSignedCountFixture 写入仅用于展示统计的合成合同记录。
// 参数：t/r为隔离测试上下文，tenant和opportunity限定关联，fixture声明来源及确认状态。
// 返回值：合同ID；不执行审批、财务、项目或外部同步。
func seedSignedCountFixture(t *testing.T, r *Repository, tenant, opportunity string,
	fixture signedCountFixture) string {
	t.Helper()
	now := time.Now().UTC()
	row := contractRecord{
		ID: ulid.Make().String(), TenantID: tenant, Title: fixture.name,
		ContractType: "直签", ServiceType: "软件测试", OwnerUserID: "test-owner",
		OpportunityID: &opportunity, AmountMinor: 10000, Currency: "CNY",
		Status: string(fixture.status), Version: 2, OriginKind: fixture.origin,
		ImportMode: string(fixture.mode), CreatedAt: now, UpdatedAt: now,
		CreatedBy: "test-actor", UpdatedBy: "test-actor",
	}
	if fixture.confirmed {
		row.ImportConfirmedAt = &now
	}
	if fixture.signed {
		date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		row.DeclaredSignedDate = &date
	}
	if fixture.policy {
		row.ImportPolicyJSON = []byte(`{"policy_version":1,"signing_mode":"DECLARED_ONLY"}`)
	}
	if err := r.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// TestSignedContractCountsIncludeConfirmedHistory 验证普通和已确认历史合同的展示统计边界。
// 参数：t为测试上下文，仅显式隔离CONTRACT_TEST_DSN运行。
// 返回值：历史归档漏计、未确认记录误计、跨租户串数或重复查询改变结果时失败。
func TestSignedContractCountsIncludeConfirmedHistory(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	cases := []signedCountFixture{
		{name: "ordinary-active", origin: "NORMAL", status: contract.StatusActive, want: 1},
		{name: "ordinary-archived", origin: "NORMAL", status: contract.StatusArchived, want: 1},
		{name: "ordinary-draft", origin: "NORMAL", status: contract.StatusDraft},
		{name: "archive-confirmed", origin: "IMPORTED", mode: contract.ImportArchiveOnly,
			status: contract.StatusArchived, confirmed: true, signed: true, policy: true, want: 1},
		{name: "closed-confirmed", origin: "IMPORTED", mode: contract.ImportHistoricalClosed,
			status: contract.StatusArchived, confirmed: true, signed: true, policy: true, want: 1},
		{name: "legacy-live-confirmed", origin: "IMPORTED", mode: contract.ImportContinueLive,
			status: contract.StatusActive, confirmed: true, signed: true, policy: true, want: 1},
		{name: "history-unconfirmed", origin: "IMPORTED", mode: contract.ImportArchiveOnly,
			status: contract.StatusArchived, signed: true, policy: true},
		{name: "history-unsigned", origin: "IMPORTED", mode: contract.ImportArchiveOnly,
			status: contract.StatusArchived, confirmed: true, policy: true},
		{name: "history-missing-policy", origin: "IMPORTED", mode: contract.ImportArchiveOnly,
			status: contract.StatusArchived, confirmed: true, signed: true},
		{name: "history-draft", origin: "IMPORTED", mode: contract.ImportArchiveOnly,
			status: contract.StatusDraft, confirmed: true, signed: true, policy: true},
		{name: "unknown-origin", origin: "UNKNOWN", mode: contract.ImportArchiveOnly,
			status: contract.StatusArchived, confirmed: true, signed: true, policy: true},
		{name: "unknown-mode", origin: "IMPORTED", mode: "UNKNOWN",
			status: contract.StatusArchived, confirmed: true, signed: true, policy: true},
		{name: "ordinary-policy-mismatch", origin: "NORMAL", status: contract.StatusActive, policy: true},
	}
	ids := make([]string, len(cases))
	for i, fixture := range cases {
		ids[i] = strconv.Itoa(700 + i)
		seedSignedCountFixture(t, r, tenant, ids[i], fixture)
	}
	// 同一商机允许多个已签合同，来源分支必须按合同去重计数且保持租户围栏。
	seedSignedCountFixture(t, r, tenant, ids[3], cases[3])
	cases[3].want = 2
	otherID := seedSignedCountFixture(t, r, ulid.Make().String(), ids[3], cases[3])
	t.Cleanup(func() { _ = r.db.Where("id=?", otherID).Delete(&contractRecord{}).Error })
	ids = append(ids, "999")
	for attempt := 0; attempt < 2; attempt++ {
		counts, err := r.CountSignedContractsByOpportunityIDs(context.Background(), tenant, ids)
		if err != nil {
			t.Fatal(err)
		}
		for i, fixture := range cases {
			if counts[ids[i]] != fixture.want {
				t.Errorf("%s count=%d want=%d", fixture.name, counts[ids[i]], fixture.want)
			}
		}
		if counts["999"] != 0 || len(counts) != len(ids) {
			t.Fatalf("零值或批量返回契约错误: count=%d size=%d", counts["999"], len(counts))
		}
	}
}
