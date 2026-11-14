// Hashcash proof-of-work for Apple Account create calls.
//
// Apple's account widget asks for this stamp on exactly three URLs
// (/account, /account/validate, /account/verification/phone). The bits and the
// challenge are not in the HTML: they arrive as response headers
// (X-Apple-HC-Bits / X-Apple-HC-Challenge) on the previous call, and the widget
// pre-computes the stamp in a background plugin. Everything below was read out
// of the widget bundle (`hashcashGenerationPlugin`, plus the `hashcashgen` UMD
// module it calls), not guessed:
//
//	stamp = "1:" + bits + ":" + <UTC yyyyMMddHHmmss> + ":" + challenge + "::" + counter
//	ok    = SHA1(stamp) has at least `bits` leading zero bits
//
// The counter is the decimal iteration index; the browser starts at 0 and the
// server recomputes the digest over the literal string it received, so any
// counter that satisfies the leading-zero-bits test is accepted.
package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// hashcashMaxBits is a guard rail: Apple currently sends 10-12, and a bad
// server value must not turn into an unbounded loop.
const hashcashMaxBits = 24

// hashcashMaxAttempts mirrors the browser budget (90 s at 1e5-1e6 H/s on a
// desktop; 2^24 is far beyond any sane difficulty).
const hashcashMaxAttempts = 1 << 24

// hashcashStamp returns a stamp satisfying the leading-zero-bits test.
func hashcashStamp(bits int, challenge string, now time.Time) (string, error) {
	challenge = strings.TrimSpace(challenge)
	switch {
	case bits <= 0:
		return "", errors.New("hashcash: server sent no challenge bits")
	case challenge == "":
		return "", errors.New("hashcash: server sent no challenge")
	case bits > hashcashMaxBits:
		return "", fmt.Errorf("hashcash: difficulty %d bits is unreasonably high", bits)
	}
	prefix := fmt.Sprintf("1:%d:%s:%s::", bits, now.UTC().Format("20060102150405"), challenge)
	for counter := 0; counter < hashcashMaxAttempts; counter++ {
		stamp := prefix + strconv.Itoa(counter)
		sum := sha1.Sum([]byte(stamp))
		if leadingZeroBits(sum[:]) >= bits {
			return stamp, nil
		}
	}
	return "", fmt.Errorf("hashcash: no stamp found within %d attempts", hashcashMaxAttempts)
}

// hashcashSatisfies re-checks a stamp the way the server does. It exists so the
// offline tests can prove the generator, not so the client can skip the work.
func hashcashSatisfies(stamp string, bits int) bool {
	sum := sha1.Sum([]byte(stamp))
	return leadingZeroBits(sum[:]) >= bits
}

func leadingZeroBits(data []byte) int {
	total := 0
	for _, b := range data {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) != 0 {
				return total
			}
			total++
		}
	}
	return total
}
