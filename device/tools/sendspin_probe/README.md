# sendspin_probe

Runs the device's Sendspin client on a host, against a real Sendspin server,
without an Echo Dot. It is how the client was checked against
[`aiosendspin`](https://github.com/Sendspin/aiosendspin) 9.1.1, the library
Music Assistant 2.10 runs.

It exercises the Noise handshake, the time filter, FLAC decode and the
scheduler end to end. It says nothing about ALSA, the DAC delay or a real WiFi
link, so it is a check on the protocol and the maths, not on the speaker.

## Run it

Two processes. The probe is the *client*: it listens and advertises, and the
server dials it.

```bash
# 1. the client, on a Linux host (or WSL): listens on :8928 for 25 s
cd device
go run ./tools/sendspin_probe -secs 25 -out ~/probe.raw -identity ~/probe.key

# 2. a real aiosendspin server, streaming a 440 Hz tone for 12 s
python -m venv v && v/bin/pip install "aiosendspin[server]==9.1.1"
v/bin/python device/tools/sendspin_probe/interop_server.py ws://localhost:8928/sendspin 12
```

`~/probe.raw` is mono S16LE at 48 kHz, one 2048-sample period per pull;
`~/probe.raw.log` has one line per pull (local µs, samples, scheduling error,
correction counters).

## Reading the result

The tone is `0.5·sin(2π·440t)·(0.6+0.4·sin(2π·2t))`, so the output can be
compared against it exactly. A healthy run is bit-exact against the generator
for as long as no correction fires, with single-frame steps where one does, no
zero runs inside the audio, and `Snaps:0 Underruns:0` in the final stats line.

## A trap that cost an afternoon

- **Run the server on Linux, or give it a clock that is not `time.monotonic()`.**
  On Windows, Python 3.12's `time.monotonic()` ticks at ~15.6 ms, which is
  timestamp noise far larger than the sync target. `interop_server.py` passes a
  `perf_counter_ns` clock for that reason.
