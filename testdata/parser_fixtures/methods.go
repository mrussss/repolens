package fixture

// Calculator performs basic math operations.
type Calculator struct {
	BaseValue int
}

// Compute adds delta to base value (value receiver).
func (c Calculator) Compute(delta int) int {
	return c.BaseValue + delta
}

// Reset clears the base value (pointer receiver).
func (c *Calculator) Reset() {
	c.BaseValue = 0
}

// Service handles business actions.
type Service struct {
	Calc *Calculator
}

// Execute runs the service calculation.
func (s *Service) Execute(val int) int {
	if s.Calc == nil {
		return val
	}
	return s.Calc.Compute(val)
}
