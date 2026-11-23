// Package session implements the encrypted tunnel session between
// two room members.
//
// Handshake (mutual, password never transmitted):
//
//	Initiator -> Responder  HELLO:   ephPubI, randI, HMAC(roomKey, ephPubI|randI)
//	Responder -> Initiator  REPLY:   ephPubR, randR, HMAC(roomKey, transcript)
//	Initiator -> Responder  CONFIRM: AEAD(peerinfo) under the session key
//	Responder -> Initiator  PEERINFO as its first AEAD packet
//
// Both tags prove knowledge of the room key, which is Argon2id of
// the password — so a wrong password on either side fails the
// handshake and the attempt is dropped without explanation (the
// design doc's "wrong password yields nothing"). An ephemeral
// X25519 exchange is mixed with the room key via HKDF, so session
// keys are unique per session and per direction, and a later
// password leak does not decrypt old sessions' recordings.
//
// After the handshake, all traffic is ChaCha20-Poly1305 AEAD with
// a per-direction key and a sequence-number nonce.
package session

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"

	"svlan/internal/room"
)

// Datagram types (first byte of every UDP packet in the protocol).
const (
	TypeHello     = 0x01 // handshake: initiator -> responder
	TypeReply     = 0x02 // handshake: responder -> initiator
	TypeData      = 0x03 // AEAD session packet
	TypeRelayWrap = 0x04 // plaintext envelope carrying a handshake datagram via a relay peer
)

// Inner payload types (first byte of decrypted session plaintext).
const (
	InnerPeerInfo = 0x10 // JSON peer info (peer id, name)
	InnerIP       = 0x11 // a raw IPv4 packet for the virtual adapter
	InnerPing     = 0x12 // tunnel-level ping (nonce + send time)
	InnerPong     = 0x13 // tunnel-level pong
	InnerGossip   = 0x14 // JSON member list
	InnerRelay    = 0x15 // relayed session packet for a third peer
)

const (
	// type | session id | peer id | ephPub | rand | tag
	helloLen = 1 + 8 + 16 + 32 + 16 + 32
	replyLen = 1 + 8 + 32 + 16 + 32
	// dataHeaderLen: type | session id | sequence
	dataHeaderLen = 1 + 8 + 8
)

// Hello is a parsed HELLO datagram. SessionID is the id the
// initiator chose for itself: the responder addresses its DATA
// packets to the initiator with it. PeerID is the initiator's
// room identity (covered by the tag, so it cannot be spoofed
// without the password).
type Hello struct {
	SessionID uint64
	PeerID    [16]byte
	EphPub    [32]byte
	Rand      [16]byte
}

// BuildHello creates a HELLO datagram and returns the initiator's
// ephemeral private key (kept until the REPLY arrives).
func BuildHello(roomKey []byte, sessionID uint64, peerID [16]byte) (datagram []byte, ephPriv [32]byte, hello Hello, err error) {
	if _, err = rand.Read(ephPriv[:]); err != nil {
		return nil, ephPriv, hello, err
	}
	pub, err := curve25519.X25519(ephPriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, ephPriv, hello, err
	}
	hello.SessionID = sessionID
	hello.PeerID = peerID
	copy(hello.EphPub[:], pub)
	if _, err = rand.Read(hello.Rand[:]); err != nil {
		return nil, ephPriv, hello, err
	}
	var sid [8]byte
	binary.BigEndian.PutUint64(sid[:], sessionID)
	tag := room.Proof(roomKey, []byte("svlan-hello-v1"), sid[:], peerID[:], hello.EphPub[:], hello.Rand[:])
	datagram = make([]byte, 0, helloLen)
	datagram = append(datagram, TypeHello)
	datagram = append(datagram, sid[:]...)
	datagram = append(datagram, peerID[:]...)
	datagram = append(datagram, hello.EphPub[:]...)
	datagram = append(datagram, hello.Rand[:]...)
	datagram = append(datagram, tag...)
	return datagram, ephPriv, hello, nil
}

// ParseHello validates a HELLO datagram against the room key. A
// false result means "not for this room / wrong password" and the
// caller must drop the attempt silently.
func ParseHello(roomKey, datagram []byte) (Hello, bool) {
	var h Hello
	if len(datagram) != helloLen || datagram[0] != TypeHello {
		return h, false
	}
	h.SessionID = binary.BigEndian.Uint64(datagram[1:9])
	copy(h.PeerID[:], datagram[9:25])
	copy(h.EphPub[:], datagram[25:57])
	copy(h.Rand[:], datagram[57:73])
	tag := datagram[73:105]
	if !room.VerifyProof(roomKey, tag, []byte("svlan-hello-v1"), datagram[1:9], h.PeerID[:], h.EphPub[:], h.Rand[:]) {
		return h, false
	}
	return h, true
}

// BuildReply answers a validated HELLO. sessionID is the
// responder's own chosen id. It returns the REPLY datagram, the
// responder's ephemeral private key and its random nonce.
func BuildReply(roomKey []byte, hello Hello, sessionID uint64) (datagram []byte, ephPriv [32]byte, randR [16]byte, err error) {
	if _, err = rand.Read(ephPriv[:]); err != nil {
		return nil, ephPriv, randR, err
	}
	pub, err := curve25519.X25519(ephPriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, ephPriv, randR, err
	}
	if _, err = rand.Read(randR[:]); err != nil {
		return nil, ephPriv, randR, err
	}
	var sid [8]byte
	binary.BigEndian.PutUint64(sid[:], sessionID)
	tag := room.Proof(roomKey, []byte("svlan-reply-v1"), sid[:], hello.EphPub[:], pub, hello.Rand[:], randR[:])
	datagram = make([]byte, 0, replyLen)
	datagram = append(datagram, TypeReply)
	datagram = append(datagram, sid[:]...)
	datagram = append(datagram, pub...)
	datagram = append(datagram, randR[:]...)
	datagram = append(datagram, tag...)
	return datagram, ephPriv, randR, nil
}

// VerifyReply checks a REPLY against the room key and the HELLO we
// sent. False = the other side does not know the password.
func VerifyReply(roomKey []byte, hello Hello, datagram []byte) (sessionID uint64, ephPubR [32]byte, randR [16]byte, ok bool) {
	if len(datagram) != replyLen || datagram[0] != TypeReply {
		return 0, ephPubR, randR, false
	}
	sessionID = binary.BigEndian.Uint64(datagram[1:9])
	copy(ephPubR[:], datagram[9:41])
	copy(randR[:], datagram[41:57])
	tag := datagram[57:89]
	if !room.VerifyProof(roomKey, tag, []byte("svlan-reply-v1"), datagram[1:9], hello.EphPub[:], ephPubR[:], hello.Rand[:], randR[:]) {
		return 0, ephPubR, randR, false
	}
	return sessionID, ephPubR, randR, true
}

// Session is an established encrypted channel to one peer.
type Session struct {
	// ID is the identifier the REMOTE side puts in datagrams it
	// sends to us (we chose it); RemoteID is what we put in ours.
	ID       uint64
	RemoteID uint64

	sendKey [32]byte
	recvKey [32]byte
	sendSeq uint64
	recvSeq uint64 // highest sequence accepted (replay window: strict >)
	aeadS   cipherAEAD
	aeadR   cipherAEAD
}

type cipherAEAD interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
	Overhead() int
}

// deriveKeys mixes the X25519 shared secret with the room key.
// initiator selects which half is "send" so both directions use
// independent keys.
func deriveKeys(shared, roomKey []byte, randI, randR [16]byte, initiator bool) (send, recv [32]byte, err error) {
	salt := append(append([]byte{}, randI[:]...), randR[:]...)
	ikm := append(append([]byte{}, shared...), roomKey...)
	// HKDF with the password key folded into the input keying
	// material: a session is bound to BOTH the ephemeral exchange
	// and the room password.
	derived, err := hkdf.Key(sha256.New, ikm, salt, "svlan-session-v1", 64)
	if err != nil {
		return send, recv, err
	}
	var keyI2R, keyR2I [32]byte
	copy(keyI2R[:], derived[:32])
	copy(keyR2I[:], derived[32:])
	if initiator {
		return keyI2R, keyR2I, nil
	}
	return keyR2I, keyI2R, nil
}

// NewSession builds the session after a completed handshake.
// localID is the session id WE chose (the remote will address its
// packets to us with it); remoteID is theirs.
func NewSession(ephPriv, ephPubRemote [32]byte, roomKey []byte, randI, randR [16]byte, initiator bool, localID, remoteID uint64) (*Session, error) {
	shared, err := curve25519.X25519(ephPriv[:], ephPubRemote[:])
	if err != nil {
		return nil, fmt.Errorf("x25519: %w", err)
	}
	send, recv, err := deriveKeys(shared, roomKey, randI, randR, initiator)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: localID, RemoteID: remoteID, sendKey: send, recvKey: recv}
	if s.aeadS, err = newAEAD(send); err != nil {
		return nil, err
	}
	if s.aeadR, err = newAEAD(recv); err != nil {
		return nil, err
	}
	return s, nil
}

func newAEAD(key [32]byte) (cipherAEAD, error) {
	return chacha20poly1305.New(key[:])
}

// Seal encrypts one inner payload into a full DATA datagram.
func (s *Session) Seal(innerType byte, payload []byte) []byte {
	s.sendSeq++
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], s.sendSeq)
	plain := make([]byte, 0, 1+len(payload))
	plain = append(plain, innerType)
	plain = append(plain, payload...)
	out := make([]byte, 0, dataHeaderLen+len(plain)+16)
	out = append(out, TypeData)
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], s.RemoteID)
	out = append(out, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], s.sendSeq)
	out = append(out, tmp[:]...)
	out = s.aeadS.Seal(out, nonce, plain, nil)
	return out
}

// Open decrypts a DATA datagram addressed to this session. The
// caller has already matched the session id in the header.
func (s *Session) Open(datagram []byte) (innerType byte, payload []byte, err error) {
	if len(datagram) < dataHeaderLen+16 || datagram[0] != TypeData {
		return 0, nil, errors.New("short data datagram")
	}
	seq := binary.BigEndian.Uint64(datagram[9:17])
	if seq <= s.recvSeq {
		return 0, nil, errors.New("replayed or out-of-order packet")
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	plain, err := s.aeadR.Open(nil, nonce, datagram[dataHeaderLen:], nil)
	if err != nil {
		return 0, nil, err
	}
	s.recvSeq = seq
	if len(plain) < 1 {
		return 0, nil, errors.New("empty plaintext")
	}
	return plain[0], plain[1:], nil
}

// DataSessionID extracts the session id a DATA datagram is
// addressed to (used for demultiplexing before decryption).
func DataSessionID(datagram []byte) (uint64, bool) {
	if len(datagram) < dataHeaderLen || datagram[0] != TypeData {
		return 0, false
	}
	return binary.BigEndian.Uint64(datagram[1:9]), true
}

// RandomSessionID picks a fresh session identifier.
func RandomSessionID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return binary.BigEndian.Uint64(b[:])
}

// CheckHMAC is re-exported for the node package's tests.
func CheckHMAC(key, tag []byte, parts ...[]byte) bool {
	return hmac.Equal(tag, room.Proof(key, parts...))
}
