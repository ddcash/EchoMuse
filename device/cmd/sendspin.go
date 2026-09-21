package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/sendspin"
	"github.com/wilbowes/EchoMuse/internal/server"
)

// Sendspin state. One manager at most, created when the controller's config
// turns the feature on and torn down when it turns it off, so a device that
// never enables it opens no port and advertises nothing.
var (
	sendspinMu      sync.Mutex
	sendspinMgr     atomic.Pointer[sendspin.Manager]
	sendspinStereo  string // the channel choice the running manager last saw
	sendspinDataDir = "/data/local/etc/echomuse"
)

// codecUnity is the codec volume index for 0dB. It mirrors server.volumeMax,
// which is unexported; server/volume.go explains why it is 127 and not the top
// of the control's range.
const codecUnity = 127

// applySendspinConfig reconciles the running Sendspin player with the current
// config. Called on every config push, so it is idempotent and cheap when
// nothing changed.
func applySendspinConfig(deviceID string, spk *speaker.PcmSpeaker, srv *server.Server) {
	snap := config.Get().Snapshot()
	want := snap.SendspinEnabled != nil && *snap.SendspinEnabled
	stereo := snap.SendspinStereoChannel

	sendspinMu.Lock()
	defer sendspinMu.Unlock()
	cur := sendspinMgr.Load()

	switch {
	case want && cur == nil:
		m, err := newSendspinManager(deviceID, srv)
		if err != nil {
			log.Printf("[sendspin] cannot start: %v", err)
			return
		}
		if err := m.Start(context.Background()); err != nil {
			log.Printf("[sendspin] cannot start: %v", err)
			return
		}
		spk.SetSyncMusic(
			func(dst []int16) int { return m.PullMusic(dst, sendspin.NowUs()) },
			m.Active,
		)
		sendspinMgr.Store(m)
		sendspinStereo = stereo
		log.Printf("[sendspin] enabled as client %s, stereo channel %q", m.ClientID(), stereo)

	case !want && cur != nil:
		// Uninstall the source before stopping the manager so the write loop
		// never pulls from a manager that is shutting down.
		spk.SetSyncMusic(nil, nil)
		cur.Stop()
		sendspinMgr.Store(nil)
		log.Printf("[sendspin] disabled")

	case want && cur != nil && stereo != sendspinStereo:
		// The channel choice picks the format order offered in client/hello, so
		// it only takes effect on a new session.
		sendspinStereo = stereo
		log.Printf("[sendspin] stereo channel now %q; reconnecting", stereo)
		cur.Reconnect()
	}
}

// notifySendspin tells a connected server the device volume changed, however
// it changed (button, Home Assistant, or the server's own command).
func notifySendspin() {
	if m := sendspinMgr.Load(); m != nil {
		m.NotifyState()
	}
}

func newSendspinManager(deviceID string, srv *server.Server) (*sendspin.Manager, error) {
	name := "EchoMuse"
	if len(deviceID) >= 4 {
		name += " " + deviceID[len(deviceID)-4:]
	}
	opts := sendspin.Options{
		Name: name,
		Device: sendspin.DeviceInfo{
			ProductName:     "Echo Dot (2nd generation)",
			Manufacturer:    "EchoMuse",
			SoftwareVersion: client.Version,
			MACAddress:      readMAC("wlan0"),
		},
		IdentityPath: sendspinDataDir + "/sendspin.key",
		Iface:        "wlan0",
		Callbacks: sendspin.Callbacks{
			Volume: func() (int, bool) {
				return sendspin.LevelToVolume(srv.VolumeLevel(), codecUnity), false
			},
			SetVolume: func(v int, _ bool) {
				srv.SetVolume(sendspin.VolumeToLevel(v, codecUnity))
			},
			SetOutputDelayMs: saveSendspinDelay,
		},
		StereoChannel:  func() string { return config.Get().Snapshot().SendspinStereoChannel },
		UnpairedAccess: func() bool { return true },
		Log:            log.Printf,
	}
	m, err := sendspin.NewManager(opts)
	if err != nil {
		return nil, err
	}
	m.SetOutputDelayMs(loadSendspinDelay())
	return m, nil
}

// The output delay is the one Sendspin setting the SERVER owns: it is set from
// Music Assistant's per-player sync control, and the spec says the client must
// persist it across reboots and reconnects. So it is stored on the device, not
// in the controller's config.
func sendspinDelayPath() string { return sendspinDataDir + "/sendspin.delay" }

func loadSendspinDelay() int {
	b, err := os.ReadFile(sendspinDelayPath())
	if err != nil {
		return 0
	}
	ms, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || ms < 0 || ms > 5000 {
		return 0
	}
	return ms
}

func saveSendspinDelay(ms int) {
	tmp := sendspinDelayPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(ms)+"\n"), 0o644); err != nil {
		log.Printf("[sendspin] cannot persist output delay: %v", err)
		return
	}
	if err := os.Rename(tmp, sendspinDelayPath()); err != nil {
		log.Printf("[sendspin] cannot persist output delay: %v", err)
	}
}

// readMAC returns the interface MAC in the lowercase colon form the spec asks
// for, or "" when it cannot be read (the field is optional).
func readMAC(iface string) string {
	b, err := os.ReadFile("/sys/class/net/" + iface + "/address")
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}
