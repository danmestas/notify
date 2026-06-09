package store

import (
	"crypto/rand"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a lexicographically sortable 26-char Crockford base32 ULID:
// 48-bit millisecond timestamp + 80 bits of randomness. No external dep.
func newULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	rand.Read(b[6:])
	return encodeULID(b)
}

// NewULID returns a new ULID string. Exported so sibling packages (trigger,
// delivery) mint primary keys in the same format the repositories use.
func NewULID() string { return newULID() }

// encodeULID renders the 16 raw bytes as 26 Crockford base32 chars.
func encodeULID(b [16]byte) string {
	var dst [26]byte
	// Timestamp: 48 bits -> first 10 chars.
	dst[0] = crockford[(b[0]&224)>>5]
	dst[1] = crockford[b[0]&31]
	dst[2] = crockford[(b[1]&248)>>3]
	dst[3] = crockford[((b[1]&7)<<2)|((b[2]&192)>>6)]
	dst[4] = crockford[(b[2]&62)>>1]
	dst[5] = crockford[((b[2]&1)<<4)|((b[3]&240)>>4)]
	dst[6] = crockford[((b[3]&15)<<1)|((b[4]&128)>>7)]
	dst[7] = crockford[(b[4]&124)>>2]
	dst[8] = crockford[((b[4]&3)<<3)|((b[5]&224)>>5)]
	dst[9] = crockford[b[5]&31]
	// Randomness: 80 bits -> remaining 16 chars.
	dst[10] = crockford[(b[6]&248)>>3]
	dst[11] = crockford[((b[6]&7)<<2)|((b[7]&192)>>6)]
	dst[12] = crockford[(b[7]&62)>>1]
	dst[13] = crockford[((b[7]&1)<<4)|((b[8]&240)>>4)]
	dst[14] = crockford[((b[8]&15)<<1)|((b[9]&128)>>7)]
	dst[15] = crockford[(b[9]&124)>>2]
	dst[16] = crockford[((b[9]&3)<<3)|((b[10]&224)>>5)]
	dst[17] = crockford[b[10]&31]
	dst[18] = crockford[(b[11]&248)>>3]
	dst[19] = crockford[((b[11]&7)<<2)|((b[12]&192)>>6)]
	dst[20] = crockford[(b[12]&62)>>1]
	dst[21] = crockford[((b[12]&1)<<4)|((b[13]&240)>>4)]
	dst[22] = crockford[((b[13]&15)<<1)|((b[14]&128)>>7)]
	dst[23] = crockford[(b[14]&124)>>2]
	dst[24] = crockford[((b[14]&3)<<3)|((b[15]&224)>>5)]
	dst[25] = crockford[b[15]&31]
	return string(dst[:])
}
