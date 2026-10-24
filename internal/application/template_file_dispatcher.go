package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// TemplateFileDelivery 标识一个不可变的模板源代次。
// 退役某代次绝不能释放较新代次的绑定。
type TemplateFileDelivery struct {
	ID, TenantID, TemplateID, FileID, Filename, ResourceType, ResourceID string
	Content                                                              []byte
	Retired                                                              bool
	UploadAttempt                                                        uint64
}

type TemplateFileLifecycleStore interface {
	// 投递及模板变更通过模板行和源行锁串行化。
	// 回调受分发器超时约束；其错误会持久化以供重试，
	// 包括网关已提交但响应丢失的情况。
	ProcessTemplateFile(context.Context, string, func(context.Context, TemplateFileDelivery) (string, error)) (bool, error)
}

type TemplateFileGateway interface {
	StampedFileGateway
	ReleaseBinding(context.Context, string, string, string, string, string) error
	ResolveUploadRequest(context.Context, string) (string, string, error)
	RetireUploadRequest(context.Context, string) (string, string, error)
}

var (
	ErrTemplateUploadPending  = errors.New("template upload is still writing")
	ErrTemplateUploadFailed   = errors.New("template upload attempt failed")
	ErrTemplateUploadRejected = errors.New("template upload content rejected")
)

func templateUploadRequestID(item TemplateFileDelivery) string {
	key := "contract-template-source-" + item.ID
	if item.UploadAttempt > 0 {
		key += "-" + strconv.FormatUint(item.UploadAttempt, 10)
	}
	return key
}

type TemplateFileDispatcher struct {
	Store         TemplateFileLifecycleStore
	Gateway       TemplateFileGateway
	ApplicationID string
	TenantID      string
	LicenseGate   LicenseChecker
	Logger        *slog.Logger
}

func (d *TemplateFileDispatcher) DeliverOnce(ctx context.Context) (bool, error) {
	if d.Store == nil || d.Gateway == nil {
		return false, fmt.Errorf("template file lifecycle is not configured")
	}
	if d.TenantID == "" {
		return false, fmt.Errorf("template file worker tenant is not configured")
	}
	service := &Service{LicenseGate: d.LicenseGate}
	return d.Store.ProcessTemplateFile(ctx, d.TenantID, func(ctx context.Context, item TemplateFileDelivery) (string, error) {
		if item.TenantID != d.TenantID {
			return item.FileID, ErrForbidden
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		fileID := item.FileID
		if item.ResourceType == "contract_template_source" {
			if item.Retired {
				resolvedID, _, err := d.Gateway.RetireUploadRequest(ctx, templateUploadRequestID(item))
				if err != nil {
					return fileID, err
				}
				if fileID != "" && resolvedID != "" && fileID != resolvedID {
					return fileID, fmt.Errorf("retired template upload file identity changed")
				}
				if resolvedID != "" {
					fileID = resolvedID
				}
				if fileID == "" {
					return "", nil
				}
				return fileID, d.Gateway.ReleaseBinding(ctx, d.ApplicationID, fileID, item.ResourceType, item.ResourceID, "TEMPLATE_SOURCE")
			}
			resolvedID, status, err := d.Gateway.ResolveUploadRequest(ctx, templateUploadRequestID(item))
			if err != nil {
				return fileID, err
			}
			if fileID != "" && resolvedID != "" && fileID != resolvedID {
				return fileID, fmt.Errorf("resolved template upload file identity changed")
			}
			if status == "NOT_FOUND" && fileID != "" {
				return fileID, fmt.Errorf("known template upload request is missing")
			}
			if status == "WRITING" {
				return resolvedID, ErrTemplateUploadPending
			}
			if status == "CANCELLED" {
				return fileID, ErrTemplateUploadRejected
			}
			if status != "NOT_FOUND" {
				fileID = resolvedID
			}
			if status == "FAILED" || status == "REJECTED" {
				if err := d.Gateway.ReleaseBinding(ctx, d.ApplicationID, fileID, item.ResourceType, item.ResourceID, "TEMPLATE_SOURCE"); err != nil {
					return fileID, err
				}
				if status == "FAILED" {
					return fileID, ErrTemplateUploadFailed
				}
				return fileID, ErrTemplateUploadRejected
			}
		}
		if item.Retired {
			return fileID, d.Gateway.ReleaseBinding(ctx, d.ApplicationID, fileID, item.ResourceType, item.ResourceID, "TEMPLATE_SOURCE")
		}
		if err := service.checkBusinessLicense(ctx); err != nil {
			return fileID, err
		}
		if fileID == "" {
			var err error
			fileID, err = d.Gateway.Upload(ctx, templateUploadRequestID(item), d.ApplicationID, "CONTRACT_TEMPLATE", item.Filename, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(item.Content))
			if err != nil {
				return "", err
			}
		}
		if err := service.checkBusinessLicense(ctx); err != nil {
			return fileID, err
		}
		return fileID, d.Gateway.Bind(ctx, d.ApplicationID, fileID, item.ResourceType, item.ResourceID, "TEMPLATE_SOURCE", item.Filename)
	})
}

func (d *TemplateFileDispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		for count := 0; count < 20; count++ {
			found, err := d.DeliverOnce(ctx)
			if err != nil && ctx.Err() == nil {
				logger := d.Logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.ErrorContext(ctx, "template file delivery failed", "error", err)
			}
			if err != nil || !found {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
