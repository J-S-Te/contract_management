package contract

import "testing"

// TestContractListQueryExpiredFilterParsesOnlyBooleanLiterals 固定 expired 参数契约：
// 只接受 true/false（忽略大小写与两端空白），其它取值表示“不筛选”而不是报错，
// 与 status/type 的宽松口径一致（端点不返回 422）。
func TestContractListQueryExpiredFilterParsesOnlyBooleanLiterals(t *testing.T) {
	cases := []struct {
		value      string
		wantFilter bool
		wantOK     bool
	}{
		{value: "true", wantFilter: true, wantOK: true},
		{value: "TRUE", wantFilter: true, wantOK: true},
		{value: " true ", wantFilter: true, wantOK: true},
		{value: "false", wantFilter: false, wantOK: true},
		{value: "False", wantFilter: false, wantOK: true},
		{value: "", wantFilter: false, wantOK: false},
		{value: "  ", wantFilter: false, wantOK: false},
		{value: "yes", wantFilter: false, wantOK: false},
		{value: "1", wantFilter: false, wantOK: false},
		{value: "0", wantFilter: false, wantOK: false},
		{value: "过期", wantFilter: false, wantOK: false},
	}
	for _, testCase := range cases {
		gotFilter, gotOK := ContractListQuery{Expired: testCase.value}.ExpiredFilter()
		if gotFilter != testCase.wantFilter || gotOK != testCase.wantOK {
			t.Fatalf("Expired=%q: (%v, %v)，want (%v, %v)",
				testCase.value, gotFilter, gotOK, testCase.wantFilter, testCase.wantOK)
		}
	}
}

// TestExpiryStatusesMatchFrontendActiveStatuses 固定“已过期”参与判定的状态集合：
// 与前端 contractBusinessTime.js 的 ACTIVE_CONTRACT_STATUSES 完全一致，
// 归档等终态不参与（归档合同的 end_date 可能已过，但不是“已过期”）。
func TestExpiryStatusesMatchFrontendActiveStatuses(t *testing.T) {
	want := []Status{StatusActive, StatusInProgress, StatusPendingPay}
	got := ExpiryStatuses()
	if len(got) != len(want) {
		t.Fatalf("ExpiryStatuses() = %v，want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ExpiryStatuses()[%d] = %q，want %q", index, got[index], want[index])
		}
	}
	for _, status := range want {
		if !status.ExpiryTracked() {
			t.Fatalf("%q 应参与过期判定", status)
		}
	}
	for _, status := range []Status{StatusDraft, StatusPending, StatusApproved, StatusCompleted, StatusTerminated, StatusArchived, Status("unknown")} {
		if status.ExpiryTracked() {
			t.Fatalf("%q 不应参与过期判定", status)
		}
	}
	// 返回值是副本：调用方改写不能污染包内集合（否则 SQL 谓词会随调用顺序漂移）。
	got[0] = StatusArchived
	if ExpiryStatuses()[0] != StatusActive {
		t.Fatal("ExpiryStatuses 必须返回副本，内部集合被改写")
	}
}
