package contract

import "testing"

// TestDiscountBucketBoundaries 验证精确整数边界。
// 参数：t为测试上下文。
// 返回值：无，分桶不符即失败。
func TestDiscountBucketBoundaries(t *testing.T) {
	for _, tc := range []struct {
		amount int64
		want   string
	}{{7999, "LT_80"}, {8000, "FROM_80_TO_90"}, {8999, "FROM_80_TO_90"}, {9000, "FROM_90_TO_100"}, {9999, "FROM_90_TO_100"}, {10000, "AT_100"}, {10001, "GT_100"}} {
		if got := DiscountBucket(tc.amount, 10000); got != tc.want {
			t.Errorf("amount=%d got %s want %s", tc.amount, got, tc.want)
		}
	}
}
