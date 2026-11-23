// Package room holds everything derived from the two things a user
// types: the room code (the public locator) and the room password
// (the actual key). Per the design doc:
//
//   - The code is hashed into the DHT lookup key. The code itself is
//     never put on the wire or into the DHT.
//   - The password is never sent anywhere. It is stretched with
//     Argon2id into a room key that the handshake proves knowledge
//     of, and that also feeds session-key derivation.
package room

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// CodeAlphabet is Crockford-style base32 with the ambiguous letters
// removed (no I, L, O, U) so a code can be read aloud over voice
// chat without confusion.
const CodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// CodeLength is the number of symbols in a room code. 10 symbols of
// base32 is ~49 bits of locator entropy — plenty for finding a
// room, and the password is the real gate anyway.
const CodeLength = 10

// GenerateCode returns a fresh random room code, formatted in two
// groups of five separated by a dash for readability ("XXXXX-XXXXX").
func GenerateCode() (string, error) {
	buf := make([]byte, CodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, c := range buf {
		if i == CodeLength/2 {
			b.WriteByte('-')
		}
		b.WriteByte(CodeAlphabet[int(c)%len(CodeAlphabet)])
	}
	return b.String(), nil
}

// NormalizeCode canonicalizes user input: upper-case, dashes and
// spaces stripped. Two people typing the same code with different
// formatting must land on the same room.
func NormalizeCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if strings.ContainsRune(CodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidCode reports whether a normalized code has the right shape.
func ValidCode(code string) bool {
	return len(NormalizeCode(code)) == CodeLength
}

// DHTKey derives the 20-byte Mainline DHT infohash for a room code.
// It is SHA-256 over a domain-separated normalized code, truncated
// to the 20 bytes a DHT key needs. The code itself never appears
// in the DHT; only this hash does.
func DHTKey(code string) [20]byte {
	sum := sha256.Sum256([]byte("svlan-room-v1:" + NormalizeCode(code)))
	var key [20]byte
	copy(key[:], sum[:20])
	return key
}

// RoomKey stretches the room password with Argon2id. The salt is
// derived from the room code, so the same password in two different
// rooms produces unrelated keys. Parameters are deliberately slow
// (design doc §6: every guess must be expensive).
func RoomKey(code, password string) []byte {
	saltSum := sha256.Sum256([]byte("svlan-salt-v1:" + NormalizeCode(code)))
	return argon2.IDKey([]byte(password), saltSum[:16], 3, 32*1024, 2, 32)
}

// Proof computes an HMAC tag proving knowledge of the room key over
// an arbitrary transcript. Used by the handshake so the password
// itself is never transmitted.
func Proof(roomKey []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, roomKey)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// VerifyProof compares a received tag in constant time.
func VerifyProof(roomKey, tag []byte, parts ...[]byte) bool {
	return hmac.Equal(tag, Proof(roomKey, parts...))
}

// --- Room-local IP assignment -------------------------------------

// Subnet is the fixed private range room members live on.
var Subnet = [4]byte{10, 88, 0, 0}

// IPForPeer deterministically derives a peer's room-local IP from
// its peer ID: hash the ID into the host part of the subnet, and
// on collision with a taken address, re-hash with a counter until
// a free slot is found. Every member can compute every other
// member's address independently — no address server, no DHCP.
func IPForPeer(peerID []byte, taken map[[4]byte]bool) [4]byte {
	for counter := uint32(0); ; counter++ {
		h := sha256.New()
		h.Write([]byte("svlan-ip-v1:"))
		h.Write(peerID)
		var cb [4]byte
		binary.BigEndian.PutUint32(cb[:], counter)
		h.Write(cb[:])
		sum := h.Sum(nil)
		ip := [4]byte{Subnet[0], Subnet[1], sum[2], sum[3]}
		// Keep clear of the network (.0) and broadcast (.255-ish)
		// corner cases on the last octet.
		if ip[3] == 0 || ip[3] == 255 {
			continue
		}
		if !taken[ip] {
			return ip
		}
	}
}

// IPString formats a room IP as dotted quad.
func IPString(ip [4]byte) string {
	return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
}

// ParseIP parses a dotted-quad back into bytes. Minimal on purpose:
// room IPs are always well-formed when they come from IPForPeer.
func ParseIP(s string) ([4]byte, bool) {
	var ip [4]byte
	var part int
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return ip, false
	}
	for i, p := range parts {
		if _, err := fmt.Sscanf(p, "%d", &part); err != nil || part < 0 || part > 255 {
			return ip, false
		}
		ip[i] = byte(part)
	}
	return ip, true
}

// InSubnet reports whether ip belongs to the room subnet.
func InSubnet(ip [4]byte) bool {
	return ip[0] == Subnet[0] && ip[1] == Subnet[1]
}

// IsBroadcast reports whether ip is the room broadcast address.
func IsBroadcast(ip [4]byte) bool {
	return ip == [4]byte{Subnet[0], Subnet[1], 255, 255}
}
