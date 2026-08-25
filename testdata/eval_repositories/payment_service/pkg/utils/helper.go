package utils

import "strings"

func SanitizeString(s string) string {
	return strings.TrimSpace(s)
}

func MaskCardNumber(card string) string {
	if len(card) < 4 {
		return "****"
	}
	return "****-****-****-" + card[len(card)-4:]
}
