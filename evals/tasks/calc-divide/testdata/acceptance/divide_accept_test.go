package calc

import (
	"errors"
	"testing"
)

// The acceptance tests are hidden from the variant under evaluation. They
// assert only what the request of the task states, and their names carry the
// prefix TestAcceptance so that they cannot collide with the tests that a
// solution adds.

func TestAcceptanceDivideQuotient(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{7, 2, 3},
		{-7, 2, -3},
		{7, -2, -3},
		{-7, -2, 3},
		{6, 3, 2},
		{0, 5, 0},
		{1, 2, 0},
	}
	for _, tt := range tests {
		got, err := Divide(tt.a, tt.b)
		if err != nil {
			t.Errorf("Divide(%d, %d) returned the error %v", tt.a, tt.b, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Divide(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAcceptanceDivideByZero(t *testing.T) {
	for _, a := range []int{1, 0, -5} {
		_, err := Divide(a, 0)
		if err == nil {
			t.Errorf("Divide(%d, 0) returned no error", a)
			continue
		}
		if !errors.Is(err, ErrDivideByZero) {
			t.Errorf("Divide(%d, 0) returned %v, which is not ErrDivideByZero", a, err)
		}
	}
}
