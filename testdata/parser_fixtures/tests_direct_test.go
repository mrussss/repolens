package fixture

import "testing"

func TestDirectUsage(t *testing.T) {
	c := Calculator{BaseValue: 10}
	res := c.Compute(5)
	if res != 15 {
		t.Fatalf("expected 15, got %d", res)
	}
	s := &Service{Calc: &c}
	_ = s.Execute(2)
}
