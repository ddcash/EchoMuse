// sendspin_probe runs the device's Sendspin client on a host, with a fake
// speaker that pulls one period every 42.67ms and records what it gets.
//
// It exists to test the client against a real Sendspin server (aiosendspin, the
// implementation Music Assistant runs) without an Echo Dot in the loop: it
// exercises the Noise handshake, the time filter, FLAC/PCM decode and the
// scheduler end to end. It says nothing about the ALSA path or the DAC delay.
//
//	go run ./tools/sendspin_probe -port 8928 -secs 20 -out /tmp/probe.raw
//
// The output is raw mono S16LE at 48kHz, one period (2048 samples) per pull,
// plus a sidecar .log of "<local_us> <real_samples>" per pull.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/wilbowes/EchoMuse/internal/sendspin"
)

func main() {
	port := flag.Int("port", 8928, "listen port")
	secs := flag.Int("secs", 20, "how long to run")
	out := flag.String("out", "/tmp/sendspin_probe.raw", "raw S16LE mono output")
	idPath := flag.String("identity", "/tmp/sendspin_probe.key", "identity key file")
	stereo := flag.String("stereo", "mono", "mono|left|right")
	delay := flag.Int("delay-ms", 0, "output delay to report")
	flag.Parse()

	m, err := sendspin.NewManager(sendspin.Options{
		Name:          "Probe",
		Device:        sendspin.DeviceInfo{ProductName: "probe", Manufacturer: "EchoMuse"},
		IdentityPath:  *idPath,
		Port:          *port,
		Iface:         "lo",
		StereoChannel: func() string { return *stereo },
		Log:           log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}
	m.SetOutputDelayMs(*delay)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*secs)*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("client_id", m.ClientID())

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	lf, _ := os.Create(*out + ".log")
	defer lf.Close()

	const period = 2048
	buf := make([]int16, period)
	raw := make([]byte, period*2)
	next := time.Now()
	step := time.Duration(period) * time.Second / 48000
	for ctx.Err() == nil {
		now := sendspin.NowUs()
		n := m.PullMusic(buf, now)
		for i, s := range buf {
			raw[i*2], raw[i*2+1] = byte(s), byte(uint16(s)>>8)
		}
		f.Write(raw)
		pst, _, _ := m.Stats()
		fmt.Fprintf(lf, "%d %d err=%d snaps=%d ins=%d drop=%d late=%d\n", now, n, pst.ErrUs, pst.Snaps, pst.Inserted, pst.Dropped, pst.LateChunks)
		next = next.Add(step)
		time.Sleep(time.Until(next))
	}
	st, errUs, buffered := m.Stats()
	fmt.Printf("stats %+v clockErrUs=%d buffered=%.2fs\n", st, errUs, buffered)
	m.Stop()
}
