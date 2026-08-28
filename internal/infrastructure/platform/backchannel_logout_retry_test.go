package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

type retryLogoutStore struct {
	called         int
	transactionErr error
}

func (s *retryLogoutStore) ProcessBackchannelLogout(context.Context, string, string, string, time.Time) (bool, error) {
	s.called++
	err := s.transactionErr
	s.transactionErr = nil
	return err == nil, err
}

func TestBackchannelLogoutRetryAfterRevokeFailure(t *testing.T) {
	store := &retryLogoutStore{transactionErr: errors.New("temporary database failure")}
	now := time.Unix(1_700_000_000, 0)
	if err := processBackchannelLogout(context.Background(), store, "tenant", "subject", "jti", now); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if err := processBackchannelLogout(context.Background(), store, "tenant", "subject", "jti", now); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if store.called != 2 {
		t.Fatalf("transaction calls = %d, want 2", store.called)
	}
}
