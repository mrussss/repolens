package fixture

// ReceiverTest struct to test receiver canonicalization.
type ReceiverTest struct {
	Value int
}

// Op1 uses value receiver.
func (r ReceiverTest) Op1() int {
	return r.Value
}

// Op2 uses pointer receiver.
func (r *ReceiverTest) Op2() {
	r.Value++
}
