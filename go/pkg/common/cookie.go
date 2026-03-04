package common

import (
	"crypto/rand"
	"math/big"
)

const DefaultCookieLen = 32

func NewCookieN(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	ret := make([]byte, n)

	for i := range n {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}

		ret[i] = letters[num.Int64()]
	}

	return string(ret), nil
}

func NewCookie() (string, error) {
	return NewCookieN(DefaultCookieLen)
}

func NewCookieMustN(n int) string {
	cookie, err := NewCookieN(n)
	if err != nil {
		panic(err)
	}

	return cookie
}

func NewCookieMust() string {
	cookie, err := NewCookie()
	if err != nil {
		panic(err)
	}

	return cookie
}
