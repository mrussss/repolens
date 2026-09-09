package repo

import (
	"context"
	"testing"
)

type fakeStore struct{}

func (f *fakeStore) Create(
	ctx context.Context,
	r *Repository,
) error {
	return nil
}

func (f *fakeStore) GetByID(
	ctx context.Context,
	id string,
) (*Repository, error) {
	return nil, nil
}

func (f *fakeStore) GetByIDAndUser(
	ctx context.Context,
	id string,
	userID string,
) (*Repository, error) {
	return nil, nil
}

func (f *fakeStore) ListByUser(
	ctx context.Context,
	userID string,
	page int,
	pageSize int,
	status string,
) ([]Repository, int64, error) {
	return nil, 0, nil
}

func (f *fakeStore) Update(
	ctx context.Context,
	r *Repository,
) error {
	return nil
}

func TestServiceRegisterDefaultRef(t *testing.T) {

	tests := []struct {
		name            string
		inputDefaultRef string
		wantDefaultRef  string
	}{
		{
			name:            "normal ref",
			inputDefaultRef: "develop",
			wantDefaultRef:  "develop",
		},
		{
			name:            "empty ref",
			inputDefaultRef: "",
			wantDefaultRef:  "main",
		},
		{
			name:            "spaces only",
			inputDefaultRef: "   ",
			wantDefaultRef:  "main",
		},
		{
			name:            "keep surrounding spaces",
			inputDefaultRef: " develop ",
			wantDefaultRef:  " develop ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(&fakeStore{})
			got, err := svc.Register(
				context.Background(),
				"user-1",
				"RepoLens",
				"https://example.com/repo.git",
				tt.inputDefaultRef,
			)
			if err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if got.DefaultRef != tt.wantDefaultRef {
				t.Fatalf(
					"expected %q, got %q",
					tt.wantDefaultRef,
					got.DefaultRef,
				)
			}
		})
	}
}
