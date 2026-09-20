package application

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// DetectionCategory 是项目管理提供给合同录入使用的受控检测类别。
type DetectionCategory struct {
	Category string `json:"category"`
	Enabled  bool   `json:"enabled"`
}

// DetectionCategoryDirectory 由项目管理维护唯一权威目录，合同管理只读使用。
type DetectionCategoryDirectory interface {
	List(context.Context) ([]DetectionCategory, error)
}

// ListDetectionCategories 返回可用于新合同服务项的启用目录。
func (s *Service) ListDetectionCategories(ctx context.Context, actor Principal) ([]DetectionCategory, error) {
	if _, ok := actor.Scope("contract.create"); !ok {
		return nil, ErrForbidden
	}
	return s.enabledDetectionCategories(ctx)
}

func (s *Service) enabledDetectionCategories(ctx context.Context) ([]DetectionCategory, error) {
	if s.DetectionCategories == nil {
		return nil, fmt.Errorf("%w: 项目管理检测类别目录未配置", ErrDetectionCategoryDirectoryUnavailable)
	}
	items, err := s.DetectionCategories.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDetectionCategoryDirectoryUnavailable, err)
	}
	result := make([]DetectionCategory, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		category := strings.TrimSpace(item.Category)
		if !item.Enabled || category == "" {
			continue
		}
		if _, exists := seen[category]; exists {
			continue
		}
		seen[category] = struct{}{}
		result = append(result, DetectionCategory{Category: category, Enabled: true})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Category < result[j].Category })
	return result, nil
}

func (s *Service) validateExternalDetectionCategories(ctx context.Context, items []string) error {
	categories, err := s.enabledDetectionCategories(ctx)
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(categories))
	for _, item := range categories {
		allowed[item.Category] = struct{}{}
	}
	for _, value := range items {
		if _, ok := allowed[strings.TrimSpace(value)]; !ok {
			return fmt.Errorf("%w: 检测类别未启用或不存在：%s", ErrValidation, strings.TrimSpace(value))
		}
	}
	return nil
}
