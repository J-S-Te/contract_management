package application

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"testing"
)

type deniedLicense struct{}

func (deniedLicense) Check(context.Context, core.Operation) error { return core.ErrDenied }

func TestBusinessDeniedBeforeRepositoryOrWorkflow(t *testing.T) {
	ctx := context.Background()
	s := &Service{LicenseGate: deniedLicense{}}
	for name, fn := range map[string]func() error{
		"create": func() error { _, err := s.CreateContract(ctx, Principal{}, contract.Contract{}); return err },
		"draft": func() error {
			_, err := s.UpdateContractDraft(ctx, Principal{}, "id", 1, contract.Contract{})
			return err
		},
		"submit": func() error { _, err := s.SubmitContractVersion(ctx, Principal{}, "id", true, 1); return err },
		"status": func() error {
			_, err := s.ChangeStatus(ctx, Principal{}, "id", 1, contract.StatusDraft, "")
			return err
		},
		"command":  func() error { _, err := s.Command(ctx, Principal{}, "id", workflows.ApprovalCommand{}); return err },
		"rule":     func() error { _, err := s.CreateRule(ctx, Principal{}, approval.Rule{}); return err },
		"template": func() error { _, err := s.CreateTemplate(ctx, Principal{}, "", "", nil); return err },
		"intake":   func() error { _, err := s.AcceptOpportunityIntake(ctx, OpportunityIntake{}); return err },
		"signing":  func() error { return s.MarkSigningReceived(ctx, Principal{}, "id") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrCommercialLicense) {
				t.Fatal(err)
			}
		})
	}
}
