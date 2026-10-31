package main

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
	max := big.NewInt(int64(len(idAlphabet)))
	id := make([]byte, idLength)
	for i := range id {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		id[i] = idAlphabet[n.Int64()]
	}
	return string(id)
}

func isValidID(id string) bool {
	return idPattern.MatchString(id)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
