package project

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
	"testing"
)

type changingGate struct{ calls int }

func (g *changingGate) Check(context.Context, core.Operation) error {
	g.calls++
	if g.calls >= 2 {
		return core.ErrDenied
	}
	return nil
}

func TestDeliveryRechecksBeforeExternalCallWithoutTerminalFailure(t *testing.T) {
	gate := &changingGate{}
	store := &memoryStore{delivery: Delivery{ID: "existing", Payload: []byte(`{}`)}}
	d := &Dispatcher{Store: store, LicenseGate: gate, BaseURL: "http://not-called.invalid"}
	if err := d.dispatchOne(context.Background()); !errors.Is(err, core.ErrDenied) {
		t.Fatal(err)
	}
	if store.failed != "" || store.delivered != "" {
		t.Fatal("license denial changed business delivery status")
	}
	if _, err := d.reconcile(context.Background()); !errors.Is(err, core.ErrDenied) || store.reconciled != 0 {
		t.Fatal("expired license enqueued historical backfill")
	}
}
