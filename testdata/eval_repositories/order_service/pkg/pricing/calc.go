package pricing

func CalculateTax(subtotal, rate float64) float64 {
	return subtotal * rate
}
