package crypto

import "crypto/sha256"

func HashKey(key string) [32]byte {
	return sha256.Sum256([]byte(key))
}
