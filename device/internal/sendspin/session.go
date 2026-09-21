package sendspin

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
	"github.com/gorilla/websocket"
)

// Wire message types. Only the ones a player uses are named.
const (
	typeClientInit  = "client/init"
	typeServerInit  = "server/init"
	typeNoise       = "noise/handshake"
	typeServerError = "server/error"
	typeServerHello = "server/hello"
	typeClientHello = "client/hello"
	typeActivate    = "server/activate"
	typeClientTime  = "client/time"
	typeServerTime  = "server/time"
	typeClientState = "client/state"
	typeCommand     = "server/command"
	typeStreamStart = "stream/start"
	typeStreamClear = "stream/clear"
	typeStreamEnd   = "stream/end"
	typeGroup       = "group/update"
	typeUnpair      = "server/unpair"
	typeGoodbye     = "client/goodbye"
	typePairAbort   = "pair/abort"
)

// Binary message IDs (first byte of a decrypted transport message).
const (
	binJSON     = 0
	binFragment = 1
	binAudio    = 4
)

// Goodbye reasons a client may send.
const (
	GoodbyeShutdown        = "shutdown"
	GoodbyeRestart         = "restart"
	GoodbyeAnotherServer   = "another_server"
	GoodbyeUserRequest     = "user_request"
	GoodbyePairingRequired = "pairing_required"
	GoodbyeConcurrent      = "concurrent_attempt"
)

const (
	handshakeTimeout        = 30 * time.Second
	writeTimeout            = 10 * time.Second
	pairMethodNotSupported  = "method_not_supported"
	rolePlayer              = "player@v1"
	activityPlayback        = "playback"
	activityPairing         = "pairing"
	maxJSONMessage          = 1 << 20
	readLimitBytes          = 4 << 20
	clockBurstCount         = 8
	clockBurstInterval      = 40 * time.Millisecond
	clockFastInterval       = 1 * time.Second
	clockSlowInterval       = 3 * time.Second
	clockFastPhase          = 30 * time.Second
	syncedAfterMeasurements = 5
)

// envelope is the JSON shape of every message.
type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Wire dialects. Music Assistant 2.10 runs aiosendspin 9.1.1, which is one
// revision behind the spec text on the Sendspin repo's main branch. The two
// differ in small, load-bearing ways: a server that cannot parse client/hello
// drops the connection, and a 13-byte audio header read as 9 (or the reverse)
// shifts every sample. So the difference is named, chosen in one place, and
// tested against the library that speaks it.
//
//	WireV9   aiosendspin 9.x: hello carries supported_commands, state carries
//	         static_delay_ms, audio chunks have a 9-byte header.
//	WireNext the spec's main branch: state carries output_delay_ms and a format
//	         preference, and audio chunks add a 4-byte send_ahead.
const (
	WireV9   = "v9"
	WireNext = "next"
)

// PlayerConfig is what the session advertises and reports as the player.
type PlayerConfig struct {
	// Wire selects the dialect; empty means WireV9.
	Wire string
	// Formats offered in client/hello, in priority order. Every format the
	// session may ever ask for MUST be listed: a server that cannot match a
	// format request against this list falls back silently and tells nobody.
	Formats []Format
	// Preferred, when set, is reported as the player's format preference.
	Preferred *Format
	// BufferCapacity is the encoded-byte budget the server may keep in flight.
	BufferCapacity int
	// RequiredLeadMs and MinBufferMs are the timing hints reported to the server.
	RequiredLeadMs int
	MinBufferMs    int
	// StereoChannel folds a stereo stream to mono: "left", "right" or "mono".
	StereoChannel string
	// SupportsMute says whether the device has an OUTPUT mute. The Echo's mute
	// button is a microphone mute, so by default it does not, and offering a
	// mute command that silences the wrong thing is worse than not offering it.
	SupportsMute bool
}

// Callbacks are how a session reaches the rest of the device. Any may be nil.
type Callbacks struct {
	// Volume returns the device's current volume (0-100) and mute state.
	Volume func() (int, bool)
	// SetVolume applies a server command. muteOnly is true for a mute command.
	SetVolume func(vol int, muted bool)
	// OutputDelayMs returns the persisted output delay; SetOutputDelayMs stores one.
	OutputDelayMs    func() int
	SetOutputDelayMs func(ms int)
	// StreamStarted / StreamStopped bracket audible playback.
	StreamStarted func()
	StreamStopped func()
	// Admit is asked when a server first activates us as a player. Returning
	// false rejects the connection (the spec's concurrent_attempt), which is
	// how one server holds the speaker against another.
	Admit func(s *Session) bool
}

// Session is one Sendspin connection. It is the client end of a server that
// dialled us, and lives exactly as long as that WebSocket.
type Session struct {
	id     Identity
	name   string
	device DeviceInfo
	unpair bool // unpaired_access.enabled
	cfg    PlayerConfig
	cb     Callbacks
	player *Player
	log    func(string, ...any)
	now    func() int64

	conn    *websocket.Conn
	wmu     sync.Mutex // serialises Encrypt+Write so nonces follow wire order
	send    *noise.CipherState
	recv    *noise.CipherState
	pskUsed string

	fmu    sync.RWMutex
	filter *TimeFilter

	// Reassembly of fragmented binary messages (read loop only).
	fragBuf  []byte
	fragType byte
	inFrag   bool

	// State touched by the read loop and read elsewhere.
	serverName   string
	activated    atomic.Bool
	roleActive   atomic.Bool // player@v1 is in active_roles
	streaming    atomic.Bool
	availableTx  atomic.Bool // we have reported available:true
	closed       atomic.Bool
	decoder      decoder
	format       Format
	dmu          sync.Mutex
	stateSeq     atomic.Uint64
	closeOnce    sync.Once
	stopCh       chan struct{}
	activeAtNs   atomic.Int64
	groupPlaying atomic.Bool
}

// DeviceInfo goes into client/hello.
type DeviceInfo struct {
	ProductName     string `json:"product_name,omitempty"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	SoftwareVersion string `json:"software_version,omitempty"`
	MACAddress      string `json:"mac_address,omitempty"`
}

// NewSession prepares a session over an accepted WebSocket. Call Run.
func NewSession(conn *websocket.Conn, id Identity, name string, dev DeviceInfo, unpairedAccess bool,
	cfg PlayerConfig, cb Callbacks, player *Player, now func() int64, logf func(string, ...any)) *Session {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Session{
		id: id, name: name, device: dev, unpair: unpairedAccess, cfg: cfg, cb: cb,
		player: player, log: logf, now: now, conn: conn,
		filter: NewTimeFilter(), stopCh: make(chan struct{}),
	}
}

// ToServer converts a local time (µs) to the server clock.
func (s *Session) ToServer(local int64) int64 {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.filter.ToServer(local)
}

// Streaming reports whether a player stream is open and audio should be pulled.
func (s *Session) Streaming() bool { return s.streaming.Load() && s.roleActive.Load() }

// Active reports whether the server has activated us as a player.
func (s *Session) Active() bool { return s.roleActive.Load() }

// ServerName is the friendly name from server/hello.
func (s *Session) ServerName() string { return s.serverName }

// Synced reports whether the clock has converged enough to report available.
func (s *Session) Synced() bool {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.filter.Synchronized() && s.filter.Count() >= syncedAfterMeasurements
}

// ClockErrUs is the time filter's offset uncertainty, for diagnostics.
func (s *Session) ClockErrUs() int64 {
	s.fmu.RLock()
	defer s.fmu.RUnlock()
	return s.filter.ErrorUs()
}

// Close ends the session, sending goodbye first when the channel is up.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.stopCh)
		if s.send != nil && reason != "" {
			_ = s.writeJSON(typeGoodbye, map[string]any{"reason": reason})
		}
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = s.conn.Close()
	})
}

// Run performs the handshake and then reads until the connection drops.
func (s *Session) Run() error {
	defer s.Close("")
	s.conn.SetReadLimit(readLimitBytes)

	// Liveness is WebSocket ping/pong, which is not encrypted. The peer pings;
	// gorilla answers automatically, so all we do is refresh the deadline.
	s.conn.SetPingHandler(func(data string) error {
		_ = s.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		return s.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
	})

	if err := s.handshake(); err != nil {
		return err
	}
	_ = s.conn.SetReadDeadline(time.Time{})

	go s.clockLoop()
	return s.readLoop()
}

// handshake runs client/init -> server/init -> Noise messages 1 and 2.
func (s *Session) handshake() error {
	// The exact bytes of both init messages are bound into the handshake.
	initBytes, err := json.Marshal(envelope{Type: typeClientInit, Payload: mustJSON(map[string]any{
		"client_id": s.id.ClientID(),
		"version":   1,
		"suite":     suiteChaChaPoly,
	})})
	if err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.TextMessage, initBytes); err != nil {
		return fmt.Errorf("send client/init: %w", err)
	}

	_ = s.conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	serverInitBytes, err := s.readText()
	if err != nil {
		return fmt.Errorf("read server/init: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(serverInitBytes, &env); err != nil {
		return fmt.Errorf("server/init: %w", err)
	}
	if env.Type == typeServerError {
		var e struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(env.Payload, &e)
		return fmt.Errorf("server refused client/init: %s", e.Reason)
	}
	if env.Type != typeServerInit {
		return fmt.Errorf("expected server/init, got %q", env.Type)
	}
	var si struct {
		ServerID string `json:"server_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(env.Payload, &si); err != nil || si.Version != 1 {
		return fmt.Errorf("server/init: unsupported version %d", si.Version)
	}

	prologue := append(append([]byte{}, initBytes...), serverInitBytes...)
	hs, err := newHandshake(s.id, si.ServerID, prologue)
	if err != nil {
		return err
	}

	msg1Bytes, err := s.readText()
	if err != nil {
		return fmt.Errorf("read noise message 1: %w", err)
	}
	var nenv envelope
	if err := json.Unmarshal(msg1Bytes, &nenv); err != nil || nenv.Type != typeNoise {
		return errors.New("expected noise/handshake after server/init")
	}
	var nd struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(nenv.Payload, &nd); err != nil {
		return err
	}
	msg1, err := b64url.DecodeString(nd.Data)
	if err != nil {
		return fmt.Errorf("noise message 1 encoding: %w", err)
	}

	msg2, sendCS, recvCS, err := hs.respond(msg1)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(envelope{Type: typeNoise, Payload: mustJSON(map[string]string{"data": b64url.EncodeToString(msg2)})})
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.TextMessage, out); err != nil {
		return fmt.Errorf("send noise message 2: %w", err)
	}
	s.send, s.recv, s.pskUsed = sendCS, recvCS, hs.pskUsed
	if hs.fellBack {
		s.log("sendspin: server referenced a credential we do not hold; continuing unpaired")
	}
	return nil
}

func (s *Session) readText() ([]byte, error) {
	mt, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if mt != websocket.TextMessage {
		return nil, errors.New("expected a text message during the handshake")
	}
	return data, nil
}

// writeJSON sends one encrypted JSON message.
func (s *Session) writeJSON(typ string, payload any) error {
	body, err := json.Marshal(envelope{Type: typ, Payload: mustJSON(payload)})
	if err != nil {
		return err
	}
	return s.writeBinary(binJSON, body)
}

// writeBinary encrypts and sends [id][body] as one WebSocket binary message.
// Everything we send is far below the 65518-byte Noise limit, so there is no
// send-side fragmentation; the read side does reassemble.
func (s *Session) writeBinary(id byte, body []byte) error {
	if len(body) > maxNoisePlaintext {
		return fmt.Errorf("message of %d bytes exceeds one Noise transport message", len(body))
	}
	plain := make([]byte, 0, len(body)+1)
	plain = append(plain, id)
	plain = append(plain, body...)

	s.wmu.Lock()
	defer s.wmu.Unlock()
	ct, err := s.send.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return s.conn.WriteMessage(websocket.BinaryMessage, ct)
}

func (s *Session) readLoop() error {
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return err
		}
		if mt != websocket.BinaryMessage {
			// A cleartext message after the switch to transport mode is a
			// silent-failure condition in the spec: close without a word.
			return errors.New("cleartext message after Noise handshake")
		}
		plain, err := s.recv.Decrypt(nil, nil, data)
		if err != nil {
			return fmt.Errorf("decrypt: %w", err)
		}
		if len(plain) == 0 {
			return errors.New("empty transport message")
		}
		if err := s.dispatch(plain[0], plain[1:]); err != nil {
			return err
		}
	}
}

// dispatch handles one decrypted message, reassembling fragments first.
func (s *Session) dispatch(id byte, body []byte) error {
	if id == binFragment {
		return s.fragment(body)
	}
	if s.inFrag {
		return errors.New("non-fragment message while a fragmented message is in flight")
	}
	return s.handle(id, body)
}

// fragment implements the spec's fragmentation: [1][flags][orig_type][data] for
// the first, [1][flags][data] after. Bit 1 marks the first fragment, bit 0 the
// last, bits 2-7 must be zero.
func (s *Session) fragment(b []byte) error {
	if len(b) < 1 {
		return errors.New("empty fragment")
	}
	flags := b[0]
	if flags&^0x03 != 0 {
		return errors.New("fragment with reserved flag bits set")
	}
	first, last := flags&0x02 != 0, flags&0x01 != 0
	data := b[1:]
	if first {
		if s.inFrag {
			return errors.New("first fragment while one is in flight")
		}
		if len(data) < 1 {
			return errors.New("first fragment without orig_type")
		}
		if data[0] == binFragment {
			return errors.New("fragment with orig_type 1")
		}
		s.inFrag, s.fragType, s.fragBuf = true, data[0], append([]byte(nil), data[1:]...)
	} else {
		if !s.inFrag {
			return errors.New("continuation fragment with none in flight")
		}
		s.fragBuf = append(s.fragBuf, data...)
	}
	if len(s.fragBuf) > readLimitBytes {
		return errors.New("fragmented message too large")
	}
	if last {
		typ, buf := s.fragType, s.fragBuf
		s.inFrag, s.fragBuf = false, nil
		return s.handle(typ, buf)
	}
	return nil
}

// handle processes a complete message. Unknown IDs are ignored, per the spec.
func (s *Session) handle(id byte, body []byte) error {
	switch id {
	case binJSON:
		return s.handleJSON(body)
	case binAudio:
		return s.handleAudio(body)
	default:
		return nil
	}
}

func (s *Session) handleJSON(body []byte) error {
	if len(body) > maxJSONMessage {
		return errors.New("oversized JSON message")
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("bad JSON message: %w", err)
	}
	switch env.Type {
	case typeServerHello:
		return s.onServerHello(env.Payload)
	case typeActivate:
		return s.onActivate(env.Payload)
	case typeServerTime:
		return s.onServerTime(env.Payload)
	case typeCommand:
		return s.onCommand(env.Payload)
	case typeStreamStart:
		return s.onStreamStart(env.Payload)
	case typeStreamClear:
		return s.onStreamClear(env.Payload)
	case typeStreamEnd:
		return s.onStreamEnd(env.Payload)
	case typeGroup:
		var g struct {
			PlaybackState string `json:"playback_state"`
		}
		if json.Unmarshal(env.Payload, &g) == nil {
			s.groupPlaying.Store(g.PlaybackState == "playing")
		}
	case typeUnpair:
		// Unpaired sessions ignore it; we hold no pairing records to drop.
	}
	// Unrecognised types are ignored.
	return nil
}

// ---- hello / activate ------------------------------------------------------

func (s *Session) onServerHello(p json.RawMessage) error {
	var h struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(p, &h)
	s.serverName = h.Name
	return s.sendHello()
}

func (s *Session) sendHello() error {
	fmts := make([]map[string]any, 0, len(s.cfg.Formats))
	for _, f := range s.cfg.Formats {
		fmts = append(fmts, map[string]any{
			"codec": f.Codec, "channels": f.Channels, "sample_rate": f.SampleRate, "bit_depth": f.BitDepth,
		})
	}
	support := map[string]any{
		"supported_formats": fmts,
		"buffer_capacity":   s.cfg.BufferCapacity,
	}
	if !s.next() {
		// aiosendspin 9.x refuses a hello without this. In the newer spec the
		// player's commands moved to client/state.
		support["supported_commands"] = s.helloCommands()
	}
	return s.writeJSON(typeClientHello, map[string]any{
		"name":              s.name,
		"device_info":       s.device,
		"supported_roles":   []string{rolePlayer},
		"player@v1_support": support,
		// Every client offers at least the Pairing PSK method. It is advertised
		// but not yet implemented: see onActivate for what happens if chosen.
		//
		// This is the aiosendspin 9.1.1 wire, which Music Assistant 2.10 runs: a
		// LIST of {method, ...} descriptors. The spec text on its main branch
		// has since moved to an object keyed by method name. Servers reject a
		// hello that does not parse, so match what is deployed, and revisit when
		// Music Assistant moves to a library that speaks the newer form.
		"supported_pair_methods": []map[string]any{
			{"method": "pairing_psk", "locations": []string{"operator"}},
		},
		"unpaired_access": map[string]any{"enabled": s.unpair},
	})
}

func (s *Session) onActivate(p json.RawMessage) error {
	var a struct {
		Activities  []string        `json:"activities"`
		ActiveRoles *[]string       `json:"active_roles"`
		Pairing     json.RawMessage `json:"pairing"`
	}
	if err := json.Unmarshal(p, &a); err != nil {
		return fmt.Errorf("server/activate: %w", err)
	}
	playback, pairing := false, false
	for _, x := range a.Activities {
		switch x {
		case activityPlayback:
			playback = true
		case activityPairing:
			pairing = true
		}
	}

	roles := []string{}
	if a.ActiveRoles != nil {
		roles = *a.ActiveRoles
	} else if s.roleActive.Load() {
		roles = []string{rolePlayer} // omitted roles persist
	}
	wantPlayer := false
	for _, r := range roles {
		if r == rolePlayer {
			wantPlayer = true
		}
	}

	// An unpaired session may only carry playback or roles when we allow it.
	if s.pskUsed != pskCategoryLongTerm && (playback || wantPlayer) && !s.unpair {
		s.log("sendspin: refusing unpaired activation (unpaired access is off)")
		s.Close(GoodbyePairingRequired)
		return errors.New("unpaired access disabled")
	}

	if pairing {
		// Pairing PSK is not implemented yet. Say so instead of stalling the
		// operator's pairing attempt; the connection stays open for playback.
		_ = s.writeJSON(typePairAbort, map[string]any{"reason": pairMethodNotSupported})
		pairing = false
	}

	if wantPlayer && !s.roleActive.Load() && s.cb.Admit != nil && !s.cb.Admit(s) {
		s.Close(GoodbyeConcurrent)
		return errors.New("another server holds the speaker")
	}

	wasActive := s.roleActive.Swap(wantPlayer)
	if wasActive && !wantPlayer {
		s.stopStream()
	}
	first := !s.activated.Swap(true)
	if wantPlayer && (!wasActive || first) {
		s.activeAtNs.Store(time.Now().UnixNano())
		s.log("sendspin: activated by %q (%s PSK)", s.serverName, s.pskUsed)
		go s.announceWhenSynced()
	}
	return nil
}

// announceWhenSynced sends the first client/state once the clock has converged.
// A player must not claim to be available before that, and the server may not
// stream until it has seen this message.
func (s *Session) announceWhenSynced() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			if !s.roleActive.Load() {
				return
			}
			if s.Synced() {
				if err := s.SendState(); err != nil {
					s.log("sendspin: send state: %v", err)
				}
				return
			}
		}
	}
}

// SendState reports the player's state. Call it whenever volume, mute or the
// output delay changes, however that change came about.
func (s *Session) SendState() error {
	if !s.roleActive.Load() || s.send == nil {
		return nil
	}
	vol, muted := 100, false
	if s.cb.Volume != nil {
		vol, muted = s.cb.Volume()
	}
	delay := 0
	if s.cb.OutputDelayMs != nil {
		delay = clampInt(s.cb.OutputDelayMs(), 0, 5000)
	}
	player := map[string]any{
		"volume":                vol,
		"required_lead_time_ms": s.cfg.RequiredLeadMs,
		"min_buffer_ms":         s.cfg.MinBufferMs,
	}
	if s.cfg.SupportsMute {
		player["muted"] = muted
	}
	if s.next() {
		player["output_delay_ms"] = delay
		player["supported_commands"] = append(s.helloCommands(), "set_output_delay")
		if pf := s.cfg.Preferred; pf != nil {
			player["format"] = map[string]any{
				"codec": pf.Codec, "channels": pf.Channels, "sample_rate": pf.SampleRate, "bit_depth": pf.BitDepth,
			}
		}
	} else {
		player["static_delay_ms"] = delay
		player["supported_commands"] = []string{"set_static_delay"}
	}
	s.availableTx.Store(true)
	return s.writeJSON(typeClientState, map[string]any{"available": true, "player": player})
}

// ---- clock -----------------------------------------------------------------

func (s *Session) clockLoop() {
	start := time.Now()
	for i := 0; i < clockBurstCount; i++ {
		if !s.sleepOrStop(clockBurstInterval) {
			return
		}
		s.sendTime()
	}
	for {
		iv := clockFastInterval
		if time.Since(start) > clockFastPhase {
			iv = clockSlowInterval
		}
		if !s.sleepOrStop(iv) {
			return
		}
		s.sendTime()
	}
}

func (s *Session) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-t.C:
		return true
	}
}

func (s *Session) sendTime() {
	if err := s.writeJSON(typeClientTime, map[string]any{"client_transmitted": s.now()}); err != nil && !s.closed.Load() {
		s.log("sendspin: send client/time: %v", err)
	}
}

func (s *Session) onServerTime(p json.RawMessage) error {
	t4 := s.now()
	var m struct {
		ClientTransmitted int64 `json:"client_transmitted"`
		ServerReceived    int64 `json:"server_received"`
		ServerTransmitted int64 `json:"server_transmitted"`
	}
	if err := json.Unmarshal(p, &m); err != nil {
		return nil
	}
	t1, t2, t3 := m.ClientTransmitted, m.ServerReceived, m.ServerTransmitted
	meas := ((t2 - t1) + (t3 - t4)) / 2
	maxErr := ((t4 - t1) - (t3 - t2)) / 2
	if maxErr < 0 {
		return nil // a negative round trip is a clock glitch, not a measurement
	}
	s.fmu.Lock()
	s.filter.Update(meas, maxErr, t4)
	s.fmu.Unlock()
	return nil
}

// ---- commands --------------------------------------------------------------

func (s *Session) onCommand(p json.RawMessage) error {
	var c struct {
		Player *struct {
			Command       string `json:"command"`
			Volume        *int   `json:"volume"`
			Mute          *bool  `json:"mute"`
			OutputDelayMs *int   `json:"output_delay_ms"`
			StaticDelayMs *int   `json:"static_delay_ms"`
		} `json:"player"`
	}
	if err := json.Unmarshal(p, &c); err != nil || c.Player == nil || !s.roleActive.Load() {
		return nil
	}
	pc := c.Player
	vol, muted := 100, false
	if s.cb.Volume != nil {
		vol, muted = s.cb.Volume()
	}
	switch pc.Command {
	case "volume":
		if pc.Volume == nil {
			return nil
		}
		vol = clampInt(*pc.Volume, 0, 100)
		if s.cb.SetVolume != nil {
			s.cb.SetVolume(vol, muted)
		}
	case "mute":
		if pc.Mute == nil || !s.cfg.SupportsMute {
			return nil
		}
		muted = *pc.Mute
		if s.cb.SetVolume != nil {
			s.cb.SetVolume(vol, muted)
		}
	case "set_output_delay", "set_static_delay":
		d := pc.OutputDelayMs
		if pc.Command == "set_static_delay" {
			d = pc.StaticDelayMs
		}
		if d == nil {
			return nil
		}
		if s.cb.SetOutputDelayMs != nil {
			s.cb.SetOutputDelayMs(clampInt(*d, 0, 5000))
		}
	default:
		return nil
	}
	return s.SendState()
}

// ---- streams ---------------------------------------------------------------

func (s *Session) onStreamStart(p json.RawMessage) error {
	var st struct {
		Player *struct {
			Codec       string `json:"codec"`
			SampleRate  int    `json:"sample_rate"`
			Channels    int    `json:"channels"`
			BitDepth    int    `json:"bit_depth"`
			CodecHeader string `json:"codec_header"`
		} `json:"player"`
	}
	if err := json.Unmarshal(p, &st); err != nil || st.Player == nil {
		return nil
	}
	if !s.roleActive.Load() {
		return nil
	}
	f := Format{Codec: st.Player.Codec, SampleRate: st.Player.SampleRate,
		Channels: st.Player.Channels, BitDepth: st.Player.BitDepth}
	if st.Player.CodecHeader != "" {
		hdr, err := base64.StdEncoding.DecodeString(st.Player.CodecHeader)
		if err != nil {
			return fmt.Errorf("stream/start codec_header: %w", err)
		}
		f.CodecHeader = hdr
	}
	if !s.offered(f) {
		// The spec says the server must pick from what we listed. If it did
		// not, decoding would be guesswork; say so loudly and stay silent.
		s.log("sendspin: server chose a format we did not offer: %+v", f)
		return nil
	}
	dec, err := newDecoder(f)
	if err != nil {
		s.log("sendspin: cannot decode stream: %v", err)
		return nil
	}
	s.dmu.Lock()
	s.decoder, s.format = dec, f
	s.dmu.Unlock()
	if !s.streaming.Swap(true) && s.cb.StreamStarted != nil {
		s.cb.StreamStarted()
	}
	s.log("sendspin: stream %s %dHz %dch %dbit", f.Codec, f.SampleRate, f.Channels, f.BitDepth)
	return nil
}

// helloCommands are the player commands offered in client/hello (v9) or as the
// base of the state-level list (next).
func (s *Session) helloCommands() []string {
	if s.cfg.SupportsMute {
		return []string{"volume", "mute"}
	}
	return []string{"volume"}
}

// next reports whether this session speaks the newer wire dialect.
func (s *Session) next() bool { return s.cfg.Wire == WireNext }

func (s *Session) offered(f Format) bool {
	for _, o := range s.cfg.Formats {
		if o.Codec == f.Codec && o.SampleRate == f.SampleRate && o.Channels == f.Channels && o.BitDepth == f.BitDepth {
			return true
		}
	}
	return false
}

func (s *Session) onStreamClear(p json.RawMessage) error {
	var c struct {
		Roles []string `json:"roles"`
	}
	_ = json.Unmarshal(p, &c)
	if len(c.Roles) > 0 {
		hit := false
		for _, r := range c.Roles {
			if r == "player" {
				hit = true
			}
		}
		if !hit {
			return nil
		}
	}
	s.player.Clear()
	return nil
}

func (s *Session) onStreamEnd(p json.RawMessage) error {
	var e struct {
		Roles []string `json:"roles"`
	}
	_ = json.Unmarshal(p, &e)
	if len(e.Roles) > 0 {
		hit := false
		for _, r := range e.Roles {
			if r == "player" {
				hit = true
			}
		}
		if !hit {
			return nil
		}
	}
	s.stopStream()
	return nil
}

func (s *Session) stopStream() {
	if s.streaming.Swap(false) {
		s.player.Clear()
		if s.cb.StreamStopped != nil {
			s.cb.StreamStopped()
		}
	}
}

// handleAudio decodes one chunk and queues it. Layout: [ts int64 BE], then on
// WireNext [send_ahead uint32 BE], then the encoded audio. send_ahead is only
// for measuring arrival delay and carries no scheduling meaning, so it is read
// past and not used.
func (s *Session) handleAudio(b []byte) error {
	hdr := 8 // timestamp
	if s.next() {
		hdr = 12 // timestamp + send_ahead
	}
	if len(b) < hdr {
		return errors.New("audio chunk shorter than its header")
	}
	if !s.streaming.Load() || !s.roleActive.Load() {
		return nil // unavailable or between streams: discard, do not close
	}
	ts := int64(binary.BigEndian.Uint64(b[0:8]))
	payload := b[hdr:]

	s.dmu.Lock()
	dec, f := s.decoder, s.format
	s.dmu.Unlock()
	if dec == nil {
		return nil
	}
	pcm, err := dec.decode(payload)
	if err != nil {
		s.log("sendspin: decode: %v", err)
		return nil
	}
	if len(pcm) == 0 {
		return nil
	}
	mono := toMono(pcm, f.Channels, s.cfg.StereoChannel)
	s.player.Push(ts, mono)
	return nil
}

// ---- helpers ---------------------------------------------------------------

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
