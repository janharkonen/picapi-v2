package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
)

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const idLength = 12

var idPattern = regexp.MustCompile(`^[0-9A-Za-z]{12}$`)

func newID() string {
	return randomString(idLength)
}

// randomString returns n characters of base62 from crypto/rand.
func randomString(n int) string {
	max := big.NewInt(int64(len(idAlphabet)))
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = idAlphabet[v.Int64()]
	}
	return string(out)
}

func isValidID(id string) bool {
	return idPattern.MatchString(id)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
