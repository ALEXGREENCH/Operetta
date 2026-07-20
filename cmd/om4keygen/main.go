// Command om4keygen creates the small-exponent RSA keys used by the Opera
// Mini 4 transport. It is a development tool: never reuse its output for an
// Internet-facing service.
package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

func main() {
	printKey("sign", 1280)
	printKey("transport", 1024)
}

func printKey(name string, bits int) {
	one := big.NewInt(1)
	e := big.NewInt(3)
	for {
		p, err := rand.Prime(rand.Reader, bits/2)
		if err != nil {
			panic(err)
		}
		q, err := rand.Prime(rand.Reader, bits-bits/2)
		if err != nil {
			panic(err)
		}
		if p.Cmp(q) == 0 {
			continue
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
		d := new(big.Int).ModInverse(e, phi)
		if d == nil {
			continue
		}
		n := new(big.Int).Mul(p, q)
		if n.BitLen() != bits {
			continue
		}
		fmt.Printf("%s modulus=%0*x\n", name, bits/4, n)
		fmt.Printf("%s private=%0*x\n", name, bits/4, d)
		return
	}
}
