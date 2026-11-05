package contract

import "testing"

// TestClampContractListOffsetLimitsOffsetToTotal 固定深页收敛公式 min(offset, total)：
// 请求 offset 超过 COUNT(*) 得到的 total 时收敛到 total（空页，页码 total/limit+1），
// 范围内的 offset 原样保留；负 offset 归零，空过滤集（total=0）不改写请求值。
func TestClampContractListOffsetLimitsOffsetToTotal(t *testing.T) {
	cases := []struct {
		name        string
		offset      int
		total       int64
		wantOffset  int
		wantPage    int
		pageSizeFor int
	}{
		{name: "深页收敛到 total", offset: 17999800, total: 45, wantOffset: 45, wantPage: 1, pageSizeFor: 200},
		{name: "范围内不变", offset: 40, total: 45, wantOffset: 40, wantPage: 3, pageSizeFor: 20},
		{name: "恰在末尾不变", offset: 45, total: 45, wantOffset: 45, wantPage: 3, pageSizeFor: 20},
		{name: "负 offset 归零", offset: -7, total: 45, wantOffset: 0, wantPage: 1, pageSizeFor: 20},
		{name: "空过滤集不改写", offset: 100, total: 0, wantOffset: 100, wantPage: 6, pageSizeFor: 20},
	}
	for _, testCase := range cases {
		got := ClampContractListOffset(testCase.offset, testCase.total)
		if got != testCase.wantOffset {
			t.Fatalf("%s: ClampContractListOffset(%d, %d) = %d，want %d",
				testCase.name, testCase.offset, testCase.total, got, testCase.wantOffset)
		}
		if page := got/testCase.pageSizeFor + 1; page != testCase.wantPage {
			t.Fatalf("%s: 页码 = %d，want %d", testCase.name, page, testCase.wantPage)
		}
	}
}
