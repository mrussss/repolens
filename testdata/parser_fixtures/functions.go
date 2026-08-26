package fixture

// Add calculates the sum of two integers.
func Add(a, b int) int {
	return a + b
}

// Divide divides numerator by denominator returning quotient and remainder.
func Divide(num, den int) (int, int, error) {
	if den == 0 {
		return 0, 0, nil
	}
	return num / den, num % den, nil
}

// SumAll calculates sum of variadic arguments.
func SumAll(nums ...int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

func unexportedHelper() string {
	return "helper"
}
