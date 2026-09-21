package sendspin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/flynn/noise"
)

// Sendspin runs every connection inside a Noise KKpsk2 channel. The SERVER is
// the Noise initiator and the CLIENT (us) is the responder, whichever side
// opened the WebSocket. Both static keys are known beforehand: they are the
// client_id and server_id, which are base64url Curve25519 public keys.
//
// Only the ChaChaPoly suite is implemented. The spec requires a client to
// support at least one and the server to support both, and ChaChaPoly is the
// software-friendly one — this device has no AES instructions.
const suiteChaChaPoly = "25519_ChaChaPoly_SHA256"

// PSK categories in the first handshake message's payload.
const (
	pskCategoryLongTerm = "lt"
	pskCategoryPairing  = "pr"
	pskCategorySentinel = "sn"
)

// sentinelPSK is SHA-256("sendspin-sentinel-psk-v1"), a published constant. It
// authenticates nothing on its own; it is the PSK input when no other applies.
// Unpaired sessions are keyed with it, and the channel is only as trustworthy
// as the LAN — the spec says so in as many words.
var sentinelPSK = sha256.Sum256([]byte("sendspin-sentinel-psk-v1"))

// pskID is base64url(SHA-256("sendspin-psk-id-v1" || PSK)), the identifier the
// server puts in Noise message 1 so the client can choose which PSK to mix in
// before it is needed.
func pskID(psk []byte) string {
	h := sha256.New()
	h.Write([]byte("sendspin-psk-id-v1"))
	h.Write(psk)
	return b64url.EncodeToString(h.Sum(nil))
}

var b64url = base64.RawURLEncoding

// Identity is the client's static Curve25519 keypair. The base64url public key
// IS the client_id, so it must be generated per device from a CSPRNG and
// persisted: the spec forbids a fixed default shared across devices, and
// servers remember a client (group membership, approval) by this value.
type Identity struct {
	Private [32]byte
	Public  [32]byte
}

// ClientID is the wire identifier: the 43-character base64url public key.
func (i Identity) ClientID() string { return b64url.EncodeToString(i.Public[:]) }

// NewIdentity draws a fresh keypair from crypto/rand.
func NewIdentity() (Identity, error) {
	kp, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity: %w", err)
	}
	var id Identity
	copy(id.Private[:], kp.Private)
	copy(id.Public[:], kp.Public)
	return id, nil
}

// IdentityFromPrivate rebuilds the keypair from a stored private key.
func IdentityFromPrivate(priv []byte) (Identity, error) {
	if len(priv) != 32 {
		return Identity{}, errors.New("identity: private key must be 32 bytes")
	}
	pub, err := noise.DH25519.DH(priv, basepoint[:])
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	copy(id.Private[:], priv)
	copy(id.Public[:], pub)
	return id, nil
}

// basepoint is the Curve25519 base point (u = 9).
var basepoint = [32]byte{9}

// handshake holds the responder side of one Noise handshake.
type handshake struct {
	hs         *noise.HandshakeState
	psks       map[string][]byte // category+psk_id -> psk
	pskUsed    string            // category of the PSK actually mixed in
	fellBack   bool
	serverPub  []byte
	suite      string
	sentinelID string
}

// newHandshake prepares a responder. prologue is the exact bytes of client/init
// followed by the exact bytes of server/init as they went over the wire — a
// re-encoding of the parsed messages would not match and fails the handshake.
func newHandshake(id Identity, serverID string, prologue []byte) (*handshake, error) {
	serverPub, err := b64url.DecodeString(serverID)
	if err != nil || len(serverPub) != 32 {
		return nil, errors.New("server_id is not a 43-character base64url public key")
	}
	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           cs,
		Random:                rand.Reader,
		Pattern:               noise.HandshakeKK,
		Initiator:             false,
		Prologue:              prologue,
		StaticKeypair:         noise.DHKey{Private: id.Private[:], Public: id.Public[:]},
		PeerStatic:            serverPub,
		PresharedKeyPlacement: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("noise init: %w", err)
	}
	h := &handshake{hs: hs, serverPub: serverPub, suite: suiteChaChaPoly, psks: map[string][]byte{}}
	h.sentinelID = pskID(sentinelPSK[:])
	h.psks[pskCategorySentinel+"/"+h.sentinelID] = sentinelPSK[:]
	return h, nil
}

// noiseMsg1Payload is the JSON inside Noise message 1.
type noiseMsg1Payload struct {
	PSKID       string `json:"psk_id"`
	PSKCategory string `json:"psk_category"`
}

// respond consumes Noise message 1 and produces message 2 plus the transport
// cipher states. The PSK is chosen from the (decrypted) message 1 payload and
// mixed in at the end of message 2, which is what psk2 means.
//
// A lookup miss falls back to the Sentinel PSK rather than failing (spec,
// "Sentinel Fallback"): the server learns from the message-2 authentication
// that we could not use the credential it referenced, and can offer re-pairing.
func (h *handshake) respond(msg1 []byte) (msg2 []byte, send, recv *noise.CipherState, err error) {
	payload, _, _, err := h.hs.ReadMessage(nil, msg1)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("noise message 1: %w", err)
	}
	var p noiseMsg1Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, nil, nil, fmt.Errorf("noise message 1 payload: %w", err)
	}
	// psk_category is in the current spec text but not in aiosendspin 9.1.1,
	// which is what Music Assistant 2.10 ships: its message 1 carries psk_id
	// alone. Accept both. Without a category the PSK is identified by id, and a
	// psk_id we do not hold is the same lookup miss either way.
	switch p.PSKCategory {
	case "", pskCategoryLongTerm, pskCategoryPairing, pskCategorySentinel:
	default:
		return nil, nil, nil, fmt.Errorf("noise message 1: unknown psk_category %q", p.PSKCategory)
	}

	psk, cat, ok := h.lookupPSK(p)
	h.pskUsed = cat
	if !ok {
		psk = sentinelPSK[:]
		h.pskUsed = pskCategorySentinel
		h.fellBack = true
	}
	if err := h.hs.SetPresharedKey(psk); err != nil {
		return nil, nil, nil, err
	}

	// Message 2's payload is the literal two bytes "{}", not an empty payload.
	msg2, c0, c1, err := h.hs.WriteMessage(nil, []byte("{}"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("noise message 2: %w", err)
	}
	if c0 == nil || c1 == nil {
		return nil, nil, nil, errors.New("noise: handshake did not complete after message 2")
	}
	// c0 is initiator->responder (we receive with it), c1 is responder->initiator.
	return msg2, c1, c0, nil
}

// lookupPSK finds the PSK a message 1 payload refers to and its category.
func (h *handshake) lookupPSK(p noiseMsg1Payload) (psk []byte, category string, ok bool) {
	if p.PSKCategory != "" {
		psk, ok = h.psks[p.PSKCategory+"/"+p.PSKID]
		return psk, p.PSKCategory, ok
	}
	for _, c := range []string{pskCategoryLongTerm, pskCategoryPairing, pskCategorySentinel} {
		if psk, ok = h.psks[c+"/"+p.PSKID]; ok {
			return psk, c, true
		}
	}
	return nil, "", false
}

// maxNoisePlaintext is the most application data one Noise transport message
// carries: 65535 - 16 (AEAD tag) - 1 (message type byte).
const maxNoisePlaintext = 65535 - 16 - 1
