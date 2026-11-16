package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// localStreamDB 只在明确回环local_fix专库运行生产迁移与指标读取验证。
// 参数：t 为测试生命周期，只读取LOCAL_METRIC_STREAM_DSN_FILE私有文件。
// 返回值：隔离数据库；未配置跳过，非专库或非私有文件连接前拒绝。
func localStreamDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := os.Getenv("LOCAL_METRIC_STREAM_DSN_FILE")
	if path == "" {
		t.Skip("explicit local stream DSN file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("DSN read failed")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host != "127.0.0.1" || cfg.Net != "tcp" || cfg.DBName != "local_fix_contract" || !cfg.ParseTime {
		t.Fatal("explicit local_fix database required")
	}
	if err := migration.Run(t.Context(), cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { pool.Close() })
	return db
}

// streamTestRows 核对帧边界并提取真实生产者输出的全部事实。
// 参数：t 为测试上下文，raw 为完整有界协议。
// 返回值：全部事实JSON；缺end、计数或摘要不符时失败。
func streamTestRows(t *testing.T, raw []byte) []json.RawMessage {
	t.Helper()
	out := []json.RawMessage{}
	digest := sha256.New()
	ended := false
	sequence := 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		if len(line) > 1<<20 {
			t.Fatal("oversized frame")
		}
		var frame struct {
			Type     string
			Sequence int
			Rows     []json.RawMessage
			Count    int    `json:"row_count"`
			SHA      string `json:"sha256"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "chunk" {
			sequence++
			if frame.Sequence != sequence || len(frame.Rows) > 256 {
				t.Fatal("invalid chunk")
			}
			for _, row := range frame.Rows {
				digest.Write(row)
				digest.Write([]byte{'\n'})
				out = append(out, row)
			}
		}
		if frame.Type == "end" {
			ended = true
			if frame.Count != len(out) || frame.SHA != hex.EncodeToString(digest.Sum(nil)) {
				t.Fatal("incomplete stream")
			}
		}
	}
	if !ended {
		t.Fatal("missing end")
	}
	return out
}

// TestLocalContractMetricStreamHistory 验证真实MySQL超过万行仍完整输出且旧客户端保留上限。
// 参数：t 为隔离测试上下文。
// 返回值：无；历史缺行、跨租户泄露或旧协议误成功时失败。
func TestLocalContractMetricStreamHistory(t *testing.T) {
	db := localStreamDB(t)
	tenant := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	defer db.Exec("DELETE FROM con_contract WHERE tenant_id=?", tenant)
	now := time.Now().UTC()
	for first := 0; first < 10001; first += 200 {
		last := first + 200
		if last > 10001 {
			last = 10001
		}
		args := []any{}
		marks := []string{}
		for i := first; i < last; i++ {
			marks = append(marks, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, fmt.Sprintf("%024d", i)+"s", tenant, fmt.Sprint(i), "metric", "test", "test", "user", int64(i), "CNY", "large body omitted", "ACTIVE", now, "user", now, "user")
		}
		query := "INSERT INTO con_contract(id,tenant_id,contract_number,title,contract_type,service_type,owner_user_id,amount_minor,currency,content,status,created_at,created_by,updated_at,updated_by) VALUES " + strings.Join(marks, ",")
		if err := db.Exec(query, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := NewRepository(db)
	var output bytes.Buffer
	if err := repo.StreamDiscountMetrics(context.Background(), tenant, &output); err != nil {
		t.Fatal(err)
	}
	rows := streamTestRows(t, output.Bytes())
	if path := os.Getenv("LOCAL_METRIC_STREAM_OUTPUT_FILE"); path != "" {
		if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(rows) != 10001 {
		t.Fatalf("rows=%d", len(rows))
	}
	if _, err := repo.DiscountMetrics(context.Background(), tenant); err == nil {
		t.Fatal("legacy limit silently lifted")
	}
	for _, raw := range rows {
		var row struct {
			Status string
			Amount int64 `json:"amount_minor"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.Status != "UNKNOWN" {
			t.Fatalf("legacy unknown changed %s", raw)
		}
	}
}

// TestLocalContractMetricProjectionPreservesPricing 验证裁剪正文后仍完整校验冻结服务项、金额、币税和基准。
// 参数：t 为显式隔离MySQL测试上下文。
// 返回值：无；遗漏冻结字段导致已知降未知或损坏定价被误认已知时失败。
func TestLocalContractMetricProjectionPreservesPricing(t *testing.T) {
	db := localStreamDB(t)
	tenant := fmt.Sprintf("price-%x", time.Now().UnixNano())
	now := time.Now().UTC()
	defer db.Exec("DELETE FROM con_contract WHERE tenant_id=?", tenant)
	pricing := contract.Pricing{BasisDate: "2026-11-01", Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, FrozenAt: now, Lines: []contract.PricingLine{{LineID: "line", ServiceID: "service", ServiceIndex: 0, PriceID: "price", PriceVersion: 1, Quantity: "1", Unit: "item", UnitPriceMinor: 10000, StandardMinor: 10000, AmountMinor: 8000}}}
	raw, _ := json.Marshal(pricing)
	items, _ := json.Marshal([]contract.ServiceItem{{SourceID: "service", ServiceType: "test"}})
	query := `INSERT INTO con_contract(id,tenant_id,contract_number,title,contract_type,service_type,owner_user_id,amount_minor,currency,content,status,created_at,created_by,updated_at,updated_by,start_date,pricing_json,service_items_json) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	if err := db.Exec(query, tenant, tenant, "one", "pricing", "test", "test", "user", 8000, "CNY", strings.Repeat("body", 256000), "ACTIVE", now, "user", now, "user", "2026-11-01", raw, items).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	out, err := repo.DiscountMetrics(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 1 || out.Rows[0].Status != "KNOWN" || out.Rows[0].StandardMinor != 10000 || out.Rows[0].Bucket != "FROM_80_TO_90" {
		t.Fatalf("pricing projection %+v", out.Rows)
	}
	pricing.Lines[0].ServiceID = "mismatch"
	raw, _ = json.Marshal(pricing)
	if err := db.Exec("UPDATE con_contract SET pricing_json=? WHERE tenant_id=?", raw, tenant).Error; err != nil {
		t.Fatal(err)
	}
	out, err = repo.DiscountMetrics(context.Background(), tenant)
	if err != nil || len(out.Rows) != 1 || out.Rows[0].UnknownReason != "INCOMPLETE_PRICING" {
		t.Fatalf("broken pricing %+v %v", out.Rows, err)
	}
}
