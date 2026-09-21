package sendspin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"
)

const (
	// DefaultPort and DefaultPath are the spec's recommended values for a
	// client that servers connect to.
	DefaultPort = 8928
	DefaultPath = "/sendspin"

	serviceType = "_sendspin._tcp"

	// speakerRate is the wire format of the device's music plane.
	speakerRate = 48000
)

// SpeakerBufferCapacity is what client/hello reports as buffer_capacity: the
// ~5.46s the speaker's own channel holds, in mono PCM bytes. The server paces
// its send-ahead against this, so it is derived from the device's real depth
// rather than guessed. FLAC needs fewer bytes per second, so a compressed
// stream can run further ahead than 5.46s; the Player caps what it holds.
const SpeakerBufferCapacity = 128 * 2048 * 2

// Options configures a Manager. The func-valued fields are read on every use so
// a config change from the controller takes effect without a restart.
type Options struct {
	Name         string
	Device       DeviceInfo
	IdentityPath string
	Port         int
	Iface        string

	// Callbacks are the device-side hooks. Admit, StreamStarted and
	// StreamStopped are wrapped: the Manager calls yours after its own.
	Callbacks Callbacks

	// Wire is the protocol dialect: WireV9 (default) or WireNext.
	Wire string

	StereoChannel  func() string // "mono" | "left" | "right"
	UnpairedAccess func() bool
	Log            func(string, ...any)
}

// Manager owns the listener, the mDNS advertisement and the one admitted
// session. It is what the speaker's write loop pulls from.
type Manager struct {
	o      Options
	id     Identity
	player *Player
	log    func(string, ...any)

	mu   sync.Mutex
	cur  *Session
	up   websocket.Upgrader
	srv  *http.Server
	mdns *zeroconf.Server
	stop context.CancelFunc

	outputDelayMs atomic.Int64
	streaming     atomic.Bool

	clock  *PlayClock
	period int64
}

var epoch = time.Now()

// NowUs is the monotonic local clock every timestamp on the client uses.
func NowUs() int64 { return time.Since(epoch).Microseconds() }

// NewManager loads or creates the client identity and prepares a Manager.
func NewManager(o Options) (*Manager, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.Iface == "" {
		o.Iface = "wlan0"
	}
	id, err := LoadOrCreateIdentity(o.IdentityPath)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		o: o, id: id, player: NewPlayer(speakerRate), log: o.Log,
		up: websocket.Upgrader{
			ReadBufferSize:  16 << 10,
			WriteBufferSize: 16 << 10,
			// Servers are not browsers; there is no origin to check.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	// Four ALSA periods of 2048 frames at 48 kHz: the write loop calls Pull one
	// ring's depth before the audio is audible.
	m.period = 2048 * 1_000_000 / speakerRate
	m.clock = NewPlayClock(m.period, 4*m.period)
	return m, nil
}

// ClientID is this device's Sendspin identifier, as servers see it.
func (m *Manager) ClientID() string { return m.id.ClientID() }

// LoadOrCreateIdentity reads the 32-byte private key at path, or generates and
// stores a new one. The key is per device and must survive reboots: the public
// half is the client_id a server remembers this speaker by.
func LoadOrCreateIdentity(path string) (Identity, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return IdentityFromPrivate(b)
	}
	id, err := NewIdentity()
	if err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Identity{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, id.Private[:], 0o600); err != nil {
		return Identity{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// Start begins listening and advertising. It returns once the listener is
// bound; the servers it serves run until Stop.
func (m *Manager) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	m.stop = cancel

	mux := http.NewServeMux()
	mux.HandleFunc(DefaultPath, m.serveWS)
	m.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", m.o.Port))
	if err != nil {
		cancel()
		return fmt.Errorf("sendspin listen: %w", err)
	}
	go func() {
		if err := m.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log("sendspin: server: %v", err)
		}
	}()
	go m.advertiseLoop(ctx)
	m.log("sendspin: listening on :%d%s as %s", m.o.Port, DefaultPath, m.id.ClientID())
	return nil
}

// Stop hangs up politely and tears down the listener.
func (m *Manager) Stop() {
	if m.stop != nil {
		m.stop()
	}
	m.mu.Lock()
	cur := m.cur
	m.mu.Unlock()
	if cur != nil {
		cur.Close(GoodbyeShutdown)
	}
	if m.srv != nil {
		_ = m.srv.Close()
	}
	if m.mdns != nil {
		m.mdns.Shutdown()
	}
}

func (m *Manager) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := m.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	stereo := "mono"
	if m.o.StereoChannel != nil {
		stereo = m.o.StereoChannel()
	}
	unpaired := true
	if m.o.UnpairedAccess != nil {
		unpaired = m.o.UnpairedAccess()
	}

	cb := m.o.Callbacks
	userStarted, userStopped, userAdmit := cb.StreamStarted, cb.StreamStopped, cb.Admit
	cb.OutputDelayMs = func() int { return int(m.outputDelayMs.Load()) }
	userSetDelay := m.o.Callbacks.SetOutputDelayMs
	cb.SetOutputDelayMs = func(ms int) {
		m.outputDelayMs.Store(int64(ms))
		if userSetDelay != nil {
			userSetDelay(ms)
		}
	}
	cb.StreamStarted = func() {
		m.streaming.Store(true)
		m.clock.Reset()
		if userStarted != nil {
			userStarted()
		}
	}
	cb.StreamStopped = func() {
		m.streaming.Store(false)
		if userStopped != nil {
			userStopped()
		}
	}
	cb.Admit = func(s *Session) bool { return m.admit(s) && (userAdmit == nil || userAdmit(s)) }

	pc := m.playerConfig(stereo)
	s := NewSession(conn, m.id, m.o.Name, m.o.Device, unpaired, pc, cb, m.player, NowUs, m.log)
	if err := s.Run(); err != nil {
		m.log("sendspin: session ended: %v", err)
	}
	m.mu.Lock()
	if m.cur == s {
		m.cur = nil
	}
	m.mu.Unlock()
	if s.Streaming() || m.streaming.Load() {
		m.streaming.Store(false)
		m.player.Clear()
		if userStopped != nil {
			userStopped()
		}
	}
}

// admit decides who holds the speaker when a second server shows up: an
// existing session that is actively a player keeps it, otherwise the newcomer
// takes over and the old one is told why.
func (m *Manager) admit(s *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && m.cur != s && m.cur.Active() {
		return false
	}
	if m.cur != nil && m.cur != s {
		old := m.cur
		go old.Close(GoodbyeAnotherServer)
	}
	m.cur = s
	return true
}

// playerConfig lists every format this device might ever ask for. A format
// request that is not on this list falls back silently on the server, so mono
// AND stereo, in both codecs, must all appear up front.
func (m *Manager) playerConfig(stereo string) PlayerConfig {
	fl := func(ch int) Format { return Format{Codec: "flac", SampleRate: speakerRate, Channels: ch, BitDepth: 16} }
	pc := func(ch int) Format { return Format{Codec: "pcm", SampleRate: speakerRate, Channels: ch, BitDepth: 16} }
	// Priority order is the format preference on WireV9 (the server takes the
	// first entry it can produce), so the channel count we actually want goes
	// first. Mono by default: the server downmixes, which halves the bitrate
	// and costs this device nothing.
	formats := []Format{fl(1), pc(1), fl(2), pc(2)}
	if stereo == "left" || stereo == "right" {
		formats = []Format{fl(2), pc(2), fl(1), pc(1)}
	}
	pref := formats[0]
	return PlayerConfig{
		Wire:           m.o.Wire,
		Formats:        formats,
		Preferred:      &pref,
		BufferCapacity: SpeakerBufferCapacity,
		RequiredLeadMs: 1000,
		MinBufferMs:    1000,
		StereoChannel:  stereo,
	}
}

// SetOutputDelayMs restores a persisted delay at startup.
func (m *Manager) SetOutputDelayMs(ms int) { m.outputDelayMs.Store(int64(clampInt(ms, 0, 5000))) }

// Active reports whether Sendspin audio should be pulled this period.
func (m *Manager) Active() bool { return m.streaming.Load() }

// PullMusic is the speaker write loop's entry point: dst is one period of mono
// S16 audio, and it is called once per period, just before the period is
// written. nowUs is the local time of that call. dst is always fully written
// (zeros where there is no audio), so a caller never mixes in stale samples;
// the return value is the count of leading samples that carry audio, 0 when
// nothing is due.
func (m *Manager) PullMusic(dst []int16, nowUs int64) int {
	m.mu.Lock()
	s := m.cur
	m.mu.Unlock()
	if s == nil || !s.Streaming() {
		clear(dst)
		return 0
	}
	playAt := m.clock.Next(nowUs) + m.outputDelayMs.Load()*1000
	return m.player.Pull(dst, s.ToServer(playAt))
}

// Stats exposes the player's counters and the clock quality for diagnostics.
func (m *Manager) Stats() (PlayerStats, int64, float64) {
	m.mu.Lock()
	s := m.cur
	m.mu.Unlock()
	var errUs int64
	if s != nil {
		errUs = s.ClockErrUs()
	}
	return m.player.Stats(), errUs, m.player.Buffered()
}

// NotifyState tells the server the device's volume or mute changed.
func (m *Manager) NotifyState() {
	m.mu.Lock()
	s := m.cur
	m.mu.Unlock()
	if s != nil {
		if err := s.SendState(); err != nil {
			m.log("sendspin: send state: %v", err)
		}
	}
}

// Reconnect ends the current session with the spec's "restart" reason, which
// tells the server to dial back in. A setting that is negotiated per session
// (the stereo channel picks the offered format order) takes effect on the next
// one, and this is how a config change gets there without dropping the
// listener.
func (m *Manager) Reconnect() {
	m.mu.Lock()
	s := m.cur
	m.mu.Unlock()
	if s != nil {
		s.Close(GoodbyeRestart)
	}
}

// ---- mDNS ------------------------------------------------------------------

// advertiseLoop keeps the _sendspin._tcp record pointing at the interface's
// current addresses. A DHCP renewal or a WiFi roam changes them, and a stale
// record makes the speaker undiscoverable in exactly the way that looks like
// "the device is not on the network".
func (m *Manager) advertiseLoop(ctx context.Context) {
	var last string
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		ips := ifaceIPs(m.o.Iface)
		key := fmt.Sprint(ips)
		if len(ips) > 0 && key != last {
			if m.mdns != nil {
				m.mdns.Shutdown()
				m.mdns = nil
			}
			if srv, err := m.register(ips); err != nil {
				m.log("sendspin: mDNS register: %v", err)
			} else {
				m.mdns, last = srv, key
				m.log("sendspin: advertising %s on %v", serviceType, ips)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) register(ips []string) (*zeroconf.Server, error) {
	host := "echomuse-" + m.id.ClientID()[:8]
	instance := m.o.Name
	txt := []string{"path=" + DefaultPath, "name=" + m.o.Name}
	var opts []net.Interface
	if ifi, err := net.InterfaceByName(m.o.Iface); err == nil {
		opts = []net.Interface{*ifi}
	}
	return zeroconf.RegisterProxy(instance, serviceType, "local.", m.o.Port, host, ips, txt, opts)
}

func ifaceIPs(name string) []string {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
