package blockchain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/sha3"
)

// ChainEthereum is the only chain supported in this version.
const ChainEthereum = "ethereum"

// ErrInvalidAddress is returned for malformed addresses.
var ErrInvalidAddress = errors.New("invalid Ethereum address")

// NormalizeAddress validates an Ethereum address and returns its EIP-55
// checksummed form, which WalletScope uses as the canonical representation for
// display and comparison. All-lowercase and all-uppercase inputs are accepted;
// mixed-case input must carry a valid EIP-55 checksum, which catches typos.
func NormalizeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) != 42 || (s[:2] != "0x" && s[:2] != "0X") {
		return "", fmt.Errorf("%w: %q must be 0x followed by 40 hex characters", ErrInvalidAddress, s)
	}
	body := s[2:]
	if _, err := hex.DecodeString(body); err != nil {
		return "", fmt.Errorf("%w: %q contains non-hex characters", ErrInvalidAddress, s)
	}
	sum := checksum(strings.ToLower(body))
	lower, upper := strings.ToLower(body), strings.ToUpper(body)
	if body != lower && body != upper && "0x"+body != sum {
		return "", fmt.Errorf("%w: %q has an invalid EIP-55 checksum (expected %s)", ErrInvalidAddress, s, sum)
	}
	return sum, nil
}

// MustNormalize is NormalizeAddress for trusted constants and tests.
func MustNormalize(s string) string {
	a, err := NormalizeAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

// IsEVMAddress reports whether s has the 0x + 40 hex shape, ignoring checksum.
func IsEVMAddress(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 42 || (s[:2] != "0x" && s[:2] != "0X") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

// normalizeLoose canonicalises provider-supplied addresses, which may not be
// checksummed consistently. It returns "" for malformed input.
func normalizeLoose(s string) string {
	if !IsEVMAddress(s) {
		return ""
	}
	return checksum(strings.ToLower(s[2:]))
}

func checksum(lowerHex string) string {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lowerHex))
	digest := hex.EncodeToString(h.Sum(nil))
	out := make([]byte, 0, 42)
	out = append(out, '0', 'x')
	for i := 0; i < len(lowerHex); i++ {
		c := lowerHex[i]
		if c >= 'a' && c <= 'f' && digest[i] >= '8' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
