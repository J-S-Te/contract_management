package crm

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/application"
	"testing"
)

type deniedLicense struct{}

func (deniedLicense) Check(context.Context, core.Operation) error { return core.ErrDenied }

func TestCRMDeliveryDeniesBeforeClaimAndCallback(t *testing.T) {
	// Nil store/client would panic if either business delivery bypassed the gate.
	d := &Dispatcher{LicenseGate: deniedLicense{}}
	if err := d.dispatchOne(context.Background()); !errors.Is(err, core.ErrDenied) {
		t.Fatal(err)
	}
	n := &LinkNotifier{LicenseGate: deniedLicense{}}
	if err := n.NotifyOpportunityLink(context.Background(), application.OpportunityIntake{}); !errors.Is(err, core.ErrDenied) {
		t.Fatal(err)
	}
}
