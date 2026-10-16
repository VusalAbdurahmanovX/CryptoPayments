package main

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
)

var decimalAmount = regexp.MustCompile(`^(0|[1-9][0-9]{0,59})(\.[0-9]{1,18})?$`)
var uint256Max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// USDT on BSC uses 18 decimals. Never use floating point for ledger amounts.
func parseAmount(s string) (*big.Int, error) {
	if !decimalAmount.MatchString(s) {
		return nil, errors.New("amount must be a positive decimal string with at most 18 decimals")
	}
	p := strings.SplitN(s, ".", 2)
	fraction := ""
	if len(p) == 2 {
		fraction = p[1]
	}
	n, ok := new(big.Int).SetString(p[0]+fraction+strings.Repeat("0", 18-len(fraction)), 10)
	if !ok || n.Sign() <= 0 || n.Cmp(uint256Max) > 0 {
		return nil, errors.New("amount out of range")
	}
	return n, nil
}

func formatAmount(n *big.Int) string {
	s := n.String()
	if len(s) <= 18 {
		s = strings.Repeat("0", 19-len(s)) + s
	}
	s = s[:len(s)-18] + "." + s[len(s)-18:]
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
