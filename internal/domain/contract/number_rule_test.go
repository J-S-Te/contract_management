package contract

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestNormalizeContractNumberRule 覆盖前缀/后缀、年份段与流水位宽的归一化校验。
func TestNormalizeContractNumberRule(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		rule      ContractNumberRule
		want      ContractNumberRule
		wantError error
		contains  string
	}{
		{name: "默认前缀年份段与位宽", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4},
			want: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}},
		{name: "去除首尾空白", rule: ContractNumberRule{Prefix: "  GTSR-  ", YearSegment: " YYYY ", SeqWidth: 4, Suffix: " /V1 "},
			want: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YYYY", SeqWidth: 4, Suffix: "/V1"}},
		{name: "空年份段归一为YYYY", rule: ContractNumberRule{Prefix: "HT-", YearSegment: "  ", SeqWidth: 4},
			want: ContractNumberRule{Prefix: "HT-", YearSegment: "YYYY", SeqWidth: 4}},
		{name: "允许白名单符号", rule: ContractNumberRule{Prefix: "GTSR-_/.", YearSegment: "NONE", SeqWidth: 1, Suffix: "-A_1.2/3"},
			want: ContractNumberRule{Prefix: "GTSR-_/.", YearSegment: "NONE", SeqWidth: 1, Suffix: "-A_1.2/3"}},
		{name: "前缀不得含中文", rule: ContractNumberRule{Prefix: "合同-", YearSegment: "YYYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "后缀不得含中文", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YYYY", SeqWidth: 4, Suffix: "-完结"},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "前缀不得含括号与井号", rule: ContractNumberRule{Prefix: "GTSR(#)-", YearSegment: "YYYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "前缀不得含内部空白", rule: ContractNumberRule{Prefix: "GT SR-", YearSegment: "YYYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "前缀不得含占位符", rule: ContractNumberRule{Prefix: "GT-{YYYY}", YearSegment: "YYYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "后缀不得含占位符", rule: ContractNumberRule{Prefix: "GT-", YearSegment: "YYYY", SeqWidth: 4, Suffix: "{ID8}"},
			wantError: ErrInvalidNumberRule, contains: "只能包含字母、数字和 - _ . /"},
		{name: "前缀超过32字符", rule: ContractNumberRule{Prefix: strings.Repeat("A", 33), YearSegment: "YYYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "前缀不能超过 32 个字符"},
		{name: "年份段未知", rule: ContractNumberRule{Prefix: "GT-", YearSegment: "YYY", SeqWidth: 4},
			wantError: ErrInvalidNumberRule, contains: "年份段只能是 NONE、YY 或 YYYY"},
		{name: "位宽为零", rule: ContractNumberRule{Prefix: "GT-", YearSegment: "YYYY", SeqWidth: 0},
			wantError: ErrInvalidNumberRule, contains: "流水位宽必须是 1 到 9 之间的整数"},
		{name: "位宽超过九", rule: ContractNumberRule{Prefix: "GT-", YearSegment: "YYYY", SeqWidth: 10},
			wantError: ErrInvalidNumberRule, contains: "流水位宽必须是 1 到 9 之间的整数"},
		{name: "最长单号超过列宽", rule: ContractNumberRule{Prefix: strings.Repeat("P", 32), YearSegment: "YYYY", SeqWidth: 9, Suffix: strings.Repeat("S", 32)},
			wantError: ErrInvalidNumberRule, contains: "超过 64 个字符上限"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := NormalizeContractNumberRule(testCase.rule)
			if testCase.wantError != nil {
				if !errors.Is(err, testCase.wantError) {
					t.Fatalf("err=%v want %v", err, testCase.wantError)
				}
				if !strings.Contains(err.Error(), testCase.contains) {
					t.Fatalf("err=%v want contains %q", err, testCase.contains)
				}
				if !strings.Contains(err.Error(), ErrInvalidNumberRule.Error()+": ") {
					t.Fatalf("错误必须是中文原因并包装校验错误: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Prefix != testCase.want.Prefix || got.Suffix != testCase.want.Suffix ||
				got.YearSegment != testCase.want.YearSegment || got.SeqWidth != testCase.want.SeqWidth {
				t.Fatalf("got %+v want %+v", got, testCase.want)
			}
		})
	}
}

// TestBuildContractNumber 覆盖三种年份段、宽度补齐、后缀与溢出边界。
func TestBuildContractNumber(t *testing.T) {
	year := 2026
	for _, testCase := range []struct {
		name      string
		rule      ContractNumberRule
		sequence  uint64
		want      string
		wantError error
	}{
		{name: "四位年份", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}, sequence: 1, want: "GTSR-260001"},
		{name: "四位年份段YYYY", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YYYY", SeqWidth: 4}, sequence: 1, want: "GTSR-20260001"},
		{name: "无年份段", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "NONE", SeqWidth: 4}, sequence: 12, want: "GTSR-0012"},
		{name: "带后缀", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "NONE", SeqWidth: 6, Suffix: "/A"}, sequence: 7, want: "GTSR-000007/A"},
		{name: "流水达到位宽上限", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}, sequence: 9999, want: "GTSR-269999"},
		{name: "流水为一时补齐", rule: ContractNumberRule{Prefix: "", YearSegment: "NONE", SeqWidth: 9}, sequence: 1, want: "000000001"},
		{name: "溢出报流水耗尽", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}, sequence: 10000, wantError: ErrNumberExhausted},
		{name: "流水为零非法", rule: ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}, sequence: 0, wantError: ErrInvalidNumberRule},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := BuildContractNumber(testCase.rule, year, testCase.sequence)
			if testCase.wantError != nil {
				if !errors.Is(err, testCase.wantError) {
					t.Fatalf("err=%v want %v", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("got %q want %q", got, testCase.want)
			}
		})
	}
}

// TestBuildContractNumberYear 验证年份段取值与年份隔离示例。
func TestBuildContractNumberYear(t *testing.T) {
	for _, testCase := range []struct {
		year int
		want string
	}{
		{year: 2026, want: "GTSR-260001"},
		{year: 2030, want: "GTSR-300001"},
		{year: 2100, want: "GTSR-000001"},
	} {
		got, err := BuildContractNumber(ContractNumberRule{Prefix: "GTSR-", YearSegment: "YY", SeqWidth: 4}, testCase.year, 1)
		if err != nil || got != testCase.want {
			t.Fatalf("year=%d got %q err=%v want %q", testCase.year, got, err, testCase.want)
		}
	}
	if got, err := BuildContractNumber(ContractNumberRule{Prefix: "GTSR-", YearSegment: "YYYY", SeqWidth: 4}, 2030, 1); err != nil || got != "GTSR-20300001" {
		t.Fatalf("YYYY got %q err=%v", got, err)
	}
}

// TestContractNumberSeqLimit 验证位宽上限换算。
func TestContractNumberSeqLimit(t *testing.T) {
	for width, want := range map[int]uint64{1: 9, 2: 99, 4: 9999, 9: 999999999, 0: 0, 10: 0} {
		if got := ContractNumberSeqLimit(width); got != want {
			t.Fatalf("width=%d got %d want %d", width, got, want)
		}
	}
}

// TestContractNumberRuleKeyIsStable 验证同一规则任何时候都得到同一指纹，
// 且不含长度偏差：固定 64 位小写十六进制。
func TestContractNumberRuleKeyIsStable(t *testing.T) {
	rule := ContractNumberRule{Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: "/A"}
	first, err := ContractNumberRuleKey(rule)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		again, err := ContractNumberRuleKey(rule)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("第 %d 次指纹 %q 与首次 %q 不一致", index, again, first)
		}
	}
	if len(first) != ContractNumberRuleKeyMaxLength {
		t.Fatalf("指纹长度=%d want %d", len(first), ContractNumberRuleKeyMaxLength)
	}
	if strings.ToLower(first) != first {
		t.Fatalf("指纹必须是小写十六进制: %q", first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("指纹不是合法十六进制: %v", err)
	}
	// 审计字段不参与指纹：只改 UpdatedBy/UpdatedAt 不换桶。
	audited := rule
	audited.UpdatedBy, audited.UpdatedAt = "operator", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if got, err := ContractNumberRuleKey(audited); err != nil || got != first {
		t.Fatalf("审计字段不应影响指纹: got=%q err=%v want=%q", got, err, first)
	}
}

// TestContractNumberRuleKeyNormalizesInput 验证等价输入（多余空白、小写年份段）
// 归一化后得到同一指纹，避免同一规则因输入格式不同而多开一个桶。
func TestContractNumberRuleKeyNormalizesInput(t *testing.T) {
	base := ContractNumberRule{Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: "/A"}
	want, err := ContractNumberRuleKey(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, rule := range map[string]ContractNumberRule{
		"前缀后缀多余空白":  {Prefix: "  GTSR-  ", YearSegment: "YY", SeqWidth: 4, Suffix: " /A "},
		"小写年份段":     {Prefix: "GTSR-", YearSegment: " yy ", SeqWidth: 4, Suffix: "/A"},
		"空年份段取YYYY": {Prefix: "GTSR-", YearSegment: " ", SeqWidth: 4, Suffix: "/A"},
	} {
		// 空年份段归一为 YYYY，与 base 的 YY 不等价，单独断言它与显式 YYYY 相同。
		if name == "空年份段取YYYY" {
			explicit, err := ContractNumberRuleKey(ContractNumberRule{Prefix: "GTSR-", YearSegment: NumberYearSegmentYYYY, SeqWidth: 4, Suffix: "/A"})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := ContractNumberRuleKey(rule); err != nil || got != explicit {
				t.Fatalf("%s: got=%q err=%v want=%q", name, got, err, explicit)
			}
			continue
		}
		got, err := ContractNumberRuleKey(rule)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s: got=%q want=%q", name, got, want)
		}
	}
}

// TestContractNumberRuleKeyChangesWithEveryField 验证每个参与拼装的字段变化都会换桶，
// 否则改规则后仍会命中旧桶而出现“看起来跳号”。
func TestContractNumberRuleKeyChangesWithEveryField(t *testing.T) {
	base := ContractNumberRule{Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: "/A"}
	baseKey, err := ContractNumberRuleKey(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, rule := range map[string]ContractNumberRule{
		"前缀变化":   {Prefix: "ABCD-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: "/A"},
		"年份段变化":  {Prefix: "GTSR-", YearSegment: NumberYearSegmentYYYY, SeqWidth: 4, Suffix: "/A"},
		"流水位宽变化": {Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 5, Suffix: "/A"},
		"后缀变化":   {Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: "/B"},
		"后缀清空":   {Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 4, Suffix: ""},
	} {
		got, err := ContractNumberRuleKey(rule)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == baseKey {
			t.Fatalf("%s 未改变指纹: %q", name, got)
		}
		// 拼装单号必须同样不同，保证“换桶”与“换号形”一致。
		builtBase, buildBaseErr := BuildContractNumber(base, 2026, 1)
		builtNew, buildNewErr := BuildContractNumber(rule, 2026, 1)
		if buildBaseErr != nil || buildNewErr != nil {
			t.Fatalf("%s: build err=%v/%v", name, buildBaseErr, buildNewErr)
		}
		if builtBase == builtNew {
			t.Fatalf("%s 指纹不同但单号相同: %q", name, builtNew)
		}
	}
	// 分隔符保护：前缀/后缀边界不同但拼接结果相同的两组规则必须得到不同指纹。
	left, err := ContractNumberRuleKey(ContractNumberRule{Prefix: "AB", YearSegment: NumberYearSegmentNone, SeqWidth: 4, Suffix: "CD"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ContractNumberRuleKey(ContractNumberRule{Prefix: "ABCD", YearSegment: NumberYearSegmentNone, SeqWidth: 4, Suffix: ""})
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("前缀/后缀边界不同必须得到不同指纹")
	}
}

// TestContractNumberRuleKeyRejectsInvalidRule 验证非法规则不产生指纹。
func TestContractNumberRuleKeyRejectsInvalidRule(t *testing.T) {
	for name, rule := range map[string]ContractNumberRule{
		"位宽为零":   {Prefix: "GTSR-", YearSegment: NumberYearSegmentYY, SeqWidth: 0},
		"年份段未知":  {Prefix: "GTSR-", YearSegment: "YYY", SeqWidth: 4},
		"前缀含占位符": {Prefix: "GT-{YYYY}", YearSegment: NumberYearSegmentYY, SeqWidth: 4},
	} {
		if key, err := ContractNumberRuleKey(rule); !errors.Is(err, ErrInvalidNumberRule) || key != "" {
			t.Fatalf("%s: key=%q err=%v want ErrInvalidNumberRule", name, key, err)
		}
	}
}
