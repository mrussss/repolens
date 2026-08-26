package fixture

// Pipeline coordinates calculator and account operations.
type Pipeline struct {
	Calc Calculator
	Acc  *Account
}

// Run executes the pipeline.
func (p *Pipeline) Run() int {
	res := p.Calc.Compute(10)
	if p.Acc != nil {
		p.Acc.Deposit(int64(res))
	}
	return res
}
