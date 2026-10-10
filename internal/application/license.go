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

// The executable always supplies a gate, including an explicit compatibility
// gate when rollout is disabled. Nil retains existing in-process callers/tests.
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
