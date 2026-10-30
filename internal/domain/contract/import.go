package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var ErrImportMixedLifecycle = errors.New("IMPORT_MIXED_LIFECYCLE")
var ErrImportManifest = errors.New("invalid import manifest")
var importKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type ImportMetadata struct {
	Title               string        `json:"title"`
	ContractType        string        `json:"contract_type"`
	ServiceType         string        `json:"service_type"`
	OpportunityID       string        `json:"opportunity_id"`
	OpportunityName     string        `json:"opportunity_name"`
	CRMCustomerID       uint64        `json:"crm_customer_id"`
	CustomerName        string        `json:"customer_name"`
	CustomerAddress     string        `json:"customer_address"`
	CustomerContact     string        `json:"customer_contact"`
	CustomerPhone       string        `json:"customer_phone"`
	Systems             []SystemInfo  `json:"systems"`
	ServiceItems        []ServiceItem `json:"service_items"`
	CustomerCreditLevel string        `json:"customer_credit_level"`
	AmountMinor         int64         `json:"amount_minor"`
	Currency            string        `json:"currency"`
	PaymentTerms        []PaymentTerm `json:"payment_terms"`
	Content             string        `json:"content"`
	StartDate           *time.Time    `json:"start_date"`
	EndDate             *time.Time    `json:"end_date"`
	ExternalPaperNumber string        `json:"external_paper_number"`
}

// Contract 转换显式白名单字段，不接受客户端归属或业务状态。
// 参数：无；使用当前清单元数据。
// 返回值：尚未校验和分配身份的合同。
func (m ImportMetadata) Contract() Contract {
	return Contract{Title: m.Title, Type: m.ContractType, ServiceType: m.ServiceType,
		OpportunityID: m.OpportunityID, OpportunityName: m.OpportunityName,
		CRMCustomerID: m.CRMCustomerID, CustomerName: m.CustomerName,
		CustomerAddress: m.CustomerAddress, CustomerContact: m.CustomerContact, CustomerPhone: m.CustomerPhone,
		Systems: m.Systems, ServiceItems: m.ServiceItems, CustomerCreditLevel: m.CustomerCreditLevel,
		AmountMinor: m.AmountMinor, Currency: m.Currency, PaymentTerms: m.PaymentTerms,
		StartDate: m.StartDate, EndDate: m.EndDate, ExternalPaperNumber: m.ExternalPaperNumber}
}

type ImportManifest struct {
	SchemaVersion int          `json:"schema_version"`
	SourceSystem  string       `json:"source_system"`
	Items         []ImportItem `json:"items"`
}
type ImportItem struct {
	Key              string                 `json:"item_key"`
	ExternalRecordID string                 `json:"external_record_id"`
	Mode             ImportMode             `json:"mode"`
	SourceFileRef    string                 `json:"source_file_ref"`
	Policy           *ImportPolicy          `json:"policy_options,omitempty"`
	Metadata         ImportMetadata         `json:"metadata"`
	HistoryEvidence  *ImportHistoryEvidence `json:"history_evidence,omitempty"`
}
type ImportIssue struct {
	Field     string `json:"field_path"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}
type ImportItemState struct {
	Item               ImportItem      `json:"item"`
	ID                 string          `json:"id"`
	State              string          `json:"state"`
	Digest             string          `json:"digest"`
	ContractID         string          `json:"contract_id,omitempty"`
	Issues             []ImportIssue   `json:"issues"`
	SourceRevision     uint64          `json:"source_revision"`
	NormalizedMetadata *ImportMetadata `json:"normalized_metadata,omitempty"`
	Targets            []ImportTarget  `json:"targets,omitempty"`
	Confirmed          bool            `json:"confirmed"`
}
type ImportSource struct {
	ID        string `json:"file_ref"`
	ItemKey   string `json:"item_key"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	FileID    string `json:"-"`
	GatewayID string `json:"gateway_id,omitempty"` // 仅持久载荷；HTTP必须清除。
	State     string `json:"state"`
}
type ImportBatch struct {
	ConfirmationHistory []ImportConfirmation `json:"confirmation_history,omitempty"`
	Confirmation        *ImportConfirmation  `json:"confirmation,omitempty"`
	CanCommit           bool                 `json:"can_commit"`
	ID                  string               `json:"batch_id"`
	TenantID            string               `json:"-"`
	ActorID             string               `json:"created_by"`
	Revision            uint64               `json:"revision"`
	CommittedRevision   uint64               `json:"committed_revision"`
	Version             uint64               `json:"version"`
	State               string               `json:"state"`
	SourceSystem        string               `json:"source_system"`
	Items               []ImportItemState    `json:"items"`
	Sources             []ImportSource       `json:"sources"`
	PreviewToken        string               `json:"preview_token,omitempty"`
	PreviewExpiresAt    time.Time            `json:"preview_expires_at"`
	ExpiresAt           time.Time            `json:"expires_at"`
	OperationID         string               `json:"operation_id,omitempty"`
	CommitKeyHash       string               `json:"-"`
	SessionHash         []byte               `json:"-"`
	Epoch               uint64               `json:"execution_epoch"`
	LeaseUntil          time.Time            `json:"-"`
}

// Validate 校验清单形状和来源键，字段业务约束交由现有合同服务。
// 参数：max为允许的批次项数。
// 返回值：重复或未知模式策略返回清单错误。
func (m ImportManifest) Validate(max int) error {
	if m.SchemaVersion != 1 || len(m.SourceSystem) > 64 || !importKey.MatchString(m.SourceSystem) || len(m.Items) == 0 || len(m.Items) > max {
		return ErrImportManifest
	}
	keys, ids := map[string]bool{}, map[string]bool{}
	normal := m.Items[0].Mode == ImportNormal
	for _, item := range m.Items {
		if (item.Mode == ImportNormal) != normal {
			return ErrImportMixedLifecycle
		}
		if !importKey.MatchString(item.Key) || !importKey.MatchString(item.ExternalRecordID) || item.SourceFileRef == "" || keys[item.Key] || ids[item.ExternalRecordID] || ValidateImportPolicy(item.Mode, item.Policy) != nil || item.Mode == "" {
			return ErrImportManifest
		}
		keys[item.Key], ids[item.ExternalRecordID] = true, true
	}
	return nil
}

// ImportDigest 生成类型化规范载荷的确定性SHA256。
// 参数：value为不包含原件字节的白名单结构。
// 返回值：十六进制SHA256或序列化错误。
func ImportDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
