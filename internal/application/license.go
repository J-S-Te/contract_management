package application

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
)

var ErrCommercialLicense = errors.New("commercial license does not permit this operation")

type LicenseChecker interface {
	Check(context.Context, core.Operation) error
}

// 可执行程序始终提供门禁，禁用灰度启用时也提供显式兼容
// 门禁。nil 保留现有进程内调用方及测试的行为。
func (s *Service) CheckLicense(ctx context.Context, op core.Operation) error {
	if s == nil || s.LicenseGate == nil {
		return nil
	}
	if err := s.LicenseGate.Check(ctx, op); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrCommercialLicense
	}
	return nil
}

func (s *Service) checkBusinessLicense(ctx context.Context) error {
	return s.CheckLicense(ctx, core.MUTATE_BUSINESS)
}
