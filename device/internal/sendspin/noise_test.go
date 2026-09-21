package sendspin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/flynn/noise"
)

// Published constants from the spec's "Pre-Shared Key" section.
func TestSentinelConstants(t *testing.T) {
	if got := hex.EncodeToString(sentinelPSK[:]); got != "1b5e24dbc1aed95fc2a5a338a90c05df44bd10f5ec1f4cd66cbf86272767b9d3" {
		t.Errorf("sentinel PSK = %s", got)
	}
	if got := pskID(sentinelPSK[:]); got != "GFsV9tLaSQm9HcFWpKsgYQOr7wFTvNUtkmFwuVz3zoo" {
		t.Errorf("sentinel psk_id = %s", got)
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.ClientID()) != 43 {
		t.Fatalf("client_id length %d, want 43", len(id.ClientID()))
	}
	again, err := IdentityFromPrivate(id.Private[:])
	if err != nil {
		t.Fatal(err)
	}
	if again.Public != id.Public {
		t.Fatal("public key not reproduced from the private key")
	}
	other, _ := NewIdentity()
	if other.Public == id.Public {
		t.Fatal("two identities collided")
	}
}

func newInitiator(t *testing.T, server, client Identity, prologue, psk []byte) *noise.HandshakeState {
	t.Helper()
	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)
	ihs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: cs, Random: rand.Reader, Pattern: noise.HandshakeKK, Initiator: true,
		Prologue:      prologue,
		StaticKeypair: noise.DHKey{Private: server.Private[:], Public: server.Public[:]},
		PeerStatic:    client.Public[:],
		PresharedKey:  psk, PresharedKeyPlacement: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ihs
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The initiator here plays the server: KKpsk2, ChaChaPoly/SHA256, prologue
// bound, PSK mixed at the end of message 2. The same construction is checked
// against aiosendspin by the interop run; this lets CI catch a regression
// without Python.
func TestHandshakeSentinelBothPayloadShapes(t *testing.T) {
	// aiosendspin 9.1.1 sends psk_id alone; the spec's main branch adds
	// psk_category. A client has to take both.
	for _, withCat := range []bool{false, true} {
		client, _ := NewIdentity()
		server, _ := NewIdentity()
		prologue := []byte(`{"type":"client/init"}{"type":"server/init"}`)
		ihs := newInitiator(t, server, client, prologue, sentinelPSK[:])

		p := map[string]string{"psk_id": pskID(sentinelPSK[:])}
		if withCat {
			p["psk_category"] = pskCategorySentinel
		}
		pl, _ := json.Marshal(p)
		msg1, _, _, err := ihs.WriteMessage(nil, pl)
		check(t, err)

		cli, err := newHandshake(client, b64url.EncodeToString(server.Public[:]), prologue)
		check(t, err)
		msg2, send, recv, err := cli.respond(msg1)
		if err != nil {
			t.Fatalf("withCategory=%v: respond: %v", withCat, err)
		}
		payload, c0, c1, err := ihs.ReadMessage(nil, msg2)
		if err != nil {
			t.Fatalf("withCategory=%v: initiator rejected message 2: %v", withCat, err)
		}
		if string(payload) != "{}" {
			t.Fatalf("message 2 payload = %q, want {}", payload)
		}
		if cli.pskUsed != pskCategorySentinel || cli.fellBack {
			t.Fatalf("pskUsed=%q fellBack=%v", cli.pskUsed, cli.fellBack)
		}

		// Transport in both directions.
		ct, err := c0.Encrypt(nil, nil, []byte("hello device"))
		check(t, err)
		pt, err := recv.Decrypt(nil, nil, ct)
		if err != nil || string(pt) != "hello device" {
			t.Fatalf("server->client: %q %v", pt, err)
		}
		ct, err = send.Encrypt(nil, nil, []byte("hello server"))
		check(t, err)
		pt, err = c1.Decrypt(nil, nil, ct)
		if err != nil || string(pt) != "hello server" {
			t.Fatalf("client->server: %q %v", pt, err)
		}
	}
}

// A server that references a credential we do not hold must land in the
// Sentinel fallback rather than fail. The initiator cannot verify message 2
// under an unknown PSK, so this checks only our half: that we fall back and
// say so, instead of aborting the handshake.
func TestHandshakeUnknownPSKFallsBackToSentinel(t *testing.T) {
	client, _ := NewIdentity()
	server, _ := NewIdentity()
	prologue := []byte("p")
	unknown := make([]byte, 32)
	unknown[0] = 7
	ihs := newInitiator(t, server, client, prologue, unknown)
	pl, _ := json.Marshal(map[string]string{"psk_id": pskID(unknown), "psk_category": pskCategoryLongTerm})
	msg1, _, _, err := ihs.WriteMessage(nil, pl)
	check(t, err)
	h, _ := newHandshake(client, b64url.EncodeToString(server.Public[:]), prologue)
	if _, _, _, err := h.respond(msg1); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if !h.fellBack || h.pskUsed != pskCategorySentinel {
		t.Fatalf("fellBack=%v pskUsed=%q", h.fellBack, h.pskUsed)
	}
}

func TestHandshakeRejectsBadPrologue(t *testing.T) {
	client, _ := NewIdentity()
	server, _ := NewIdentity()
	ihs := newInitiator(t, server, client, []byte("what the server saw"), sentinelPSK[:])
	pl, _ := json.Marshal(map[string]string{"psk_id": pskID(sentinelPSK[:])})
	msg1, _, _, _ := ihs.WriteMessage(nil, pl)
	h, _ := newHandshake(client, b64url.EncodeToString(server.Public[:]), []byte("what the client saw"))
	if _, _, _, err := h.respond(msg1); err == nil {
		t.Fatal("a tampered init exchange must fail the handshake")
	}
}

func TestHandshakeRejectsUnknownCategory(t *testing.T) {
	client, _ := NewIdentity()
	server, _ := NewIdentity()
	ihs := newInitiator(t, server, client, []byte("p"), sentinelPSK[:])
	pl, _ := json.Marshal(map[string]string{"psk_id": pskID(sentinelPSK[:]), "psk_category": "xx"})
	msg1, _, _, _ := ihs.WriteMessage(nil, pl)
	h, _ := newHandshake(client, b64url.EncodeToString(server.Public[:]), []byte("p"))
	if _, _, _, err := h.respond(msg1); err == nil {
		t.Fatal("a psk_category outside lt/pr/sn is a malformed payload")
	}
}
