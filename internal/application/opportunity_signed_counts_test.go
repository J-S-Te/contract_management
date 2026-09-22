package application

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type signedCountRepository struct {
	*recordingRepository
	tenant string
	ids    []string
	result map[string]uint64
}

func (repository *signedCountRepository) CountSignedContractsByOpportunityIDs(_ context.Context, tenant string, ids []string) (map[string]uint64, error) {
	repository.tenant = tenant
	repository.ids = append([]string(nil), ids...)
	return repository.result, nil
}

func TestCountSignedContractsByOpportunityIDs(t *testing.T) {
	repository := &signedCountRepository{recordingRepository: &recordingRepository{}, result: map[string]uint64{"7": 2, "9": 0}}
	service := &Service{Repo: repository}
	got, err := service.CountSignedContractsByOpportunityIDs(context.Background(), " tenant-1 ", []string{"7", "9"})
	if err != nil {
		t.Fatalf("CountSignedContractsByOpportunityIDs() error = %v", err)
	}
	if repository.tenant != "tenant-1" || !reflect.DeepEqual(repository.ids, []string{"7", "9"}) || !reflect.DeepEqual(got, repository.result) {
		t.Fatalf("unexpected call/result: tenant=%q ids=%v result=%v", repository.tenant, repository.ids, got)
	}
}

func TestCountSignedContractsByOpportunityIDsRejectsInvalidInputAndMissingCapability(t *testing.T) {
	service := &Service{Repo: &recordingRepository{}}
	if _, err := service.CountSignedContractsByOpportunityIDs(context.Background(), "", []string{"7"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant error = %v, want ErrValidation", err)
	}
	if _, err := service.CountSignedContractsByOpportunityIDs(context.Background(), "tenant-1", []string{"7"}); !errors.Is(err, ErrSignedContractCounterUnavailable) {
		t.Fatalf("missing counter error = %v, want ErrSignedContractCounterUnavailable", err)
	}
}
