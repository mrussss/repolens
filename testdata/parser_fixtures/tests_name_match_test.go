package fixture

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fail()
	}
}

func TestCalculator_Compute(t *testing.T) {
	c := Calculator{BaseValue: 1}
	_ = c.Compute(2)
}

func BenchmarkAdd(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Add(1, 2)
	}
}

// TestDivide follows naming convention without direct invocation inside body
func TestDivide(t *testing.T) {
	t.Log("testing division edge cases")
}

