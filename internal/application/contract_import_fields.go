package application

import (
	"fmt"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"strings"
	"unicode/utf8"
)

// importFieldIssues 收集可独立定位的字段错误，权威目录仍在预检服务实时复核。
// 参数：item为清单白名单项。
// 返回值：不含输入原值的字段路径及错误码。
func importFieldIssues(item contract.ImportItem) []contract.ImportIssue {
	issues := []contract.ImportIssue{}
	m := item.Metadata
	add := func(field, code string) { issues = append(issues, contract.ImportIssue{Field: field, Code: code}) }
	if item.Mode == contract.ImportHistoricalClosed && item.Policy != nil && item.Policy.FinanceMode == "FULL_REPLAY" {
		add("policy_options.finance_mode", "IMPORT_INACTIVE_RESPONSIBILITY_UNSUPPORTED")
	}
	if contract.ValidateHistoryEvidence(item) != nil {
		add("history_evidence", "IMPORT_EVIDENCE_UNSUPPORTED")
	}
	if strings.TrimSpace(m.Title) == "" {
		add("metadata.title", "REQUIRED")
	}
	if m.ContractType != "直签" && m.ContractType != "三方" {
		add("metadata.contract_type", "UNSUPPORTED_ENUM")
	}
	if m.CRMCustomerID == 0 {
		add("metadata.crm_customer_id", "REQUIRED")
	}
	if strings.TrimSpace(m.CustomerName) == "" {
		add("metadata.customer_name", "REQUIRED")
	}
	if m.AmountMinor <= 0 {
		add("metadata.amount_minor", "POSITIVE_MINOR_UNITS_REQUIRED")
	}
	if (m.OpportunityID == "") != (m.OpportunityName == "") {
		add("metadata.opportunity_id", "OPPORTUNITY_PAIR_REQUIRED")
	}
	if len(m.ServiceItems) < 1 || len(m.ServiceItems) > 20 {
		add("metadata.service_items", "ITEM_COUNT_INVALID")
	}
	if utf8.RuneCountInString(m.ExternalPaperNumber) > 128 {
		add("metadata.external_paper_number", "TOO_LONG")
	}
	for index, service := range m.ServiceItems {
		prefix := fmt.Sprintf("metadata.service_items[%d]", index)
		for _, field := range []struct{ name, value string }{{"service_type", service.ServiceType}, {"site", service.Site}, {"batch", service.Batch}, {"category", service.Category}, {"test_mode", service.TestMode}} {
			if strings.TrimSpace(field.value) == "" {
				add(prefix+"."+field.name, "REQUIRED")
			}
		}
		if len(service.Systems) > 15 {
			add(prefix+".systems", "ITEM_COUNT_INVALID")
		}
		for n, system := range service.Systems {
			if strings.TrimSpace(system.Name) == "" {
				add(fmt.Sprintf("%s.systems[%d].name", prefix, n), "REQUIRED")
			}
			if system.Level != "一级" && system.Level != "二级" && system.Level != "三级" && system.Level != "四级" {
				add(fmt.Sprintf("%s.systems[%d].level", prefix, n), "UNSUPPORTED_ENUM")
			}
		}
	}
	if m.StartDate != nil && m.EndDate != nil && m.StartDate.After(*m.EndDate) {
		add("metadata.end_date", "DATE_ORDER_INVALID")
	}
	if _, err := contract.NormalizePaymentTerms(m.PaymentTerms, m.AmountMinor, m.Currency); err != nil {
		add("metadata.payment_terms", "PAYMENT_TERMS_INVALID")
	}
	return issues
}

// normalizedImportMetadata 返回实际目录解析后的白名单字段，不将输入姓名冒充权威映射。
// 参数：c为预检已规范化的合同。
// 返回值：无原件字节和内部文件身份的规范元数据。
func normalizedImportMetadata(c contract.Contract) *contract.ImportMetadata {
	return &contract.ImportMetadata{Title: c.Title,
		ContractType:        c.Type,
		ServiceType:         c.ServiceType,
		OpportunityID:       c.OpportunityID,
		OpportunityName:     c.OpportunityName,
		CRMCustomerID:       c.CRMCustomerID,
		CustomerName:        c.CustomerName,
		CustomerAddress:     c.CustomerAddress,
		CustomerContact:     c.CustomerContact,
		CustomerPhone:       c.CustomerPhone,
		Systems:             c.Systems,
		ServiceItems:        c.ServiceItems,
		CustomerCreditLevel: c.CustomerCreditLevel,
		AmountMinor:         c.AmountMinor,
		Currency:            c.Currency,
		PaymentTerms:        c.PaymentTerms,
		StartDate:           c.StartDate,
		EndDate:             c.EndDate,
		ExternalPaperNumber: c.ExternalPaperNumber}
}
