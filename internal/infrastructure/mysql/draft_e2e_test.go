package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Opt-in only: never derive this DSN from application configuration or business
// database environment variables. The dedicated database must already exist.
func draftE2ERepository(t *testing.T) (*Repository, string) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" || !(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target an explicitly named contract_test_* database on loopback TCP")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal("dedicated test database migrations failed")
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("dedicated test database connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)
	tenant := ulid.Make().String()
	t.Cleanup(func() {
		for _, model := range []any{&lifecycleEventRecord{}, &approvalInstanceRecord{}, &contractRecord{}} {
			if err := db.Where("tenant_id = ?", tenant).Delete(model).Error; err != nil {
				t.Error("dedicated test tenant cleanup failed")
			}
		}
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewRepository(db), tenant
}

func draftE2EContract(t *testing.T, r *Repository, tenant string) contract.Contract {
	t.Helper()
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "test original", Type: "直签", ServiceType: "软件测试", TemplateID: ulid.Make().String(), OwnerUserID: ulid.Make().String(), OwnerIdentityID: ulid.Make().String(), OwnerOrgID: ulid.Make().String(), ProjectID: ulid.Make().String(), Currency: "CNY", AmountMinor: 1000000, Content: "original body", ContentHash: draftE2EHash("original body"), Status: contract.StatusDraft, Version: 1, Document: []byte("original document"), SourceFileID: "original-file", SourceFileStatus: "READY"}
	if err := r.CreateContract(context.Background(), c, c.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	c, err := r.GetContract(context.Background(), tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func draftE2EHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestDraftPersistenceE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	original := draftE2EContract(t, r, tenant)
	changed := original
	changed.Title = "edited"
	changed.Content = "edited body"
	changed.Document = []byte("edited document")
	changed.ContentHash = draftE2EHash("edited document")
	// Even an internal DTO cannot change immutable storage columns.
	changed.OwnerUserID = "forged-owner"
	changed.CreatedBy = "forged-creator"
	changed.Number = "forged-number"
	changed.SourceFileID = "forged-file"
	actor := ulid.Make().String()
	if err := r.UpdateContractDraft(ctx, changed, 1, actor); err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetContract(ctx, tenant, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Title != "edited" || stored.Content != "edited body" || string(stored.Document) != "edited document" || stored.ContentHash != changed.ContentHash {
		t.Fatal("edited draft payload or version was not persisted")
	}
	if stored.OwnerUserID != original.OwnerUserID || stored.CreatedBy != original.CreatedBy || stored.CreatedAt != original.CreatedAt || stored.Number != original.Number || stored.OwnerIdentityID != original.OwnerIdentityID || stored.OwnerOrgID != original.OwnerOrgID || stored.ProjectID != original.ProjectID || stored.SourceFileID != original.SourceFileID || stored.TemplateID != original.TemplateID {
		t.Fatal("immutable draft field changed")
	}
	var count int64
	if err := r.db.Model(&lifecycleEventRecord{}).Where("tenant_id = ? AND contract_id = ? AND reason = ? AND actor_user_id = ?", tenant, original.ID, "contract draft edited", actor).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("edit audit count %d", count)
	}
	if err := r.UpdateContractDraft(ctx, changed, 1, actor); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale edit error %v", err)
	}
	if err := r.db.Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, original.ID).Update("status", contract.StatusPending).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateContractDraft(ctx, changed, 2, actor); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("pending edit error %v", err)
	}
}

func TestDraftEditApprovalRaceE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	for iteration := 0; iteration < 10; iteration++ {
		original := draftE2EContract(t, r, tenant)
		changed := original
		changed.Content = "new snapshot"
		changed.Document = []byte("new snapshot")
		changed.ContentHash = draftE2EHash("new snapshot")
		in := workflows.StartApprovalActivityInput{ApprovalID: ulid.Make().String(), TenantID: tenant, ContractID: original.ID, ApplicantUserID: original.OwnerUserID, ExpectedVersion: 1, Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending, ContentHash: original.ContentHash, WorkflowID: "draft-race-" + original.ID, RunID: ulid.Make().String()}
		start := make(chan struct{})
		editResult := make(chan error, 1)
		approvalResult := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		go func() { <-start; editResult <- r.UpdateContractDraft(ctx, changed, 1, original.OwnerUserID) }()
		go func() { <-start; approvalResult <- r.StartApproval(ctx, in) }()
		close(start)
		editErr, approvalErr := <-editResult, <-approvalResult
		cancel()
		if (editErr == nil) == (approvalErr == nil) {
			t.Fatalf("exactly one snapshot must win: edit=%v approval=%v", editErr, approvalErr)
		}
		stored, err := r.GetContract(context.Background(), tenant, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != 2 {
			t.Fatalf("race version %d", stored.Version)
		}
		if editErr == nil {
			if !errors.Is(approvalErr, apperrors.ErrVersionConflict) || stored.Status != contract.StatusDraft || stored.ContentHash != changed.ContentHash {
				t.Fatal("edit winner did not block stale approval")
			}
		} else if !errors.Is(editErr, apperrors.ErrVersionConflict) || stored.Status != contract.StatusPending || stored.ContentHash != original.ContentHash {
			t.Fatal("approval winner did not freeze original snapshot")
		}
	}
}
