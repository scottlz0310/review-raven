package reviewrequest

import (
	"crypto/rand"
	"time"
)

// Crockford base32, lowercase: sortable by creation time and matching the
// contract's ^[prefix]_[0-9a-z]{26}$ identifiers.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// Identifier prefixes.
const (
	prefixCycle     = "cyc"
	prefixCancel    = "cxl"
	idRandomChars   = 16
	idTimestampChar = 10
)

// NewID returns a ULID-like identifier "<prefix>_<10 time chars><16 random chars>".
// Cycle IDs are never reused: the time prefix plus 80 random bits make a
// collision (and therefore reuse of an expired URI) practically impossible.
func NewID(prefix string) string {
	buf := make([]byte, 0, len(prefix)+1+idTimestampChar+idRandomChars)
	buf = append(buf, prefix...)
	buf = append(buf, '_')

	ms := uint64(time.Now().UnixMilli())
	ts := make([]byte, idTimestampChar)
	for i := idTimestampChar - 1; i >= 0; i-- {
		ts[i] = idAlphabet[ms&31]
		ms >>= 5
	}
	buf = append(buf, ts...)

	random := make([]byte, idRandomChars)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand failing means the platform entropy source is broken;
		// continuing would hand out predictable identifiers.
		panic("reviewrequest: crypto/rand failed: " + err.Error())
	}
	for _, b := range random {
		buf = append(buf, idAlphabet[b&31])
	}
	return string(buf)
}
