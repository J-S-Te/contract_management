package mysql

import (
	"context"
	"strings"
)

func (r *Repository) MarkContractSourceGatewayResult(ctx context.Context, tenantID, contractID, fileID, state, detail string) error {
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return r.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenantID, contractID).Updates(map[string]any{
		"source_file_id": strings.TrimSpace(fileID), "source_file_status": strings.TrimSpace(state), "source_file_last_error": strings.TrimSpace(detail),
	}).Error
}

func (r *Repository) MarkTemplateGatewayResult(ctx context.Context, tenantID, templateID, fileID, state, detail string) error {
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return r.db.WithContext(ctx).Model(&contractTemplateRecord{}).Where("tenant_id = ? AND id = ?", tenantID, templateID).Updates(map[string]any{
		"platform_file_id": strings.TrimSpace(fileID), "file_gateway_state": strings.TrimSpace(state), "file_gateway_last_error": strings.TrimSpace(detail),
	}).Error
}
