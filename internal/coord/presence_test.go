package coord

import (
	"strconv"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

func TestBoundStatusConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		wantCount int
		truncated bool
	}{
		{name: "empty"},
		{name: "under limit", count: StatusConflictLimit - 1, wantCount: StatusConflictLimit - 1},
		{name: "at limit", count: StatusConflictLimit, wantCount: StatusConflictLimit},
		{name: "over limit", count: StatusConflictLimit + 1, wantCount: StatusConflictLimit, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := make([]privacy.ConflictView, tc.count)
			for i := range input {
				input[i].ID = domain.ID(strconv.Itoa(i))
			}

			got, total, truncated := boundStatusConflicts(input)
			if len(got) != tc.wantCount {
				t.Fatalf("returned %d conflicts, want %d", len(got), tc.wantCount)
			}
			if total != tc.count {
				t.Fatalf("total = %d, want %d", total, tc.count)
			}
			if truncated != tc.truncated {
				t.Fatalf("truncated = %t, want %t", truncated, tc.truncated)
			}
			for i := range got {
				if got[i].ID != input[i].ID {
					t.Fatalf("conflict %d = %q, want %q", i, got[i].ID, input[i].ID)
				}
			}
		})
	}
}
