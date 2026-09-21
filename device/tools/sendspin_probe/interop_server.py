"""Drive a real aiosendspin 9.x server at the Go client and stream a test tone."""
import asyncio, math, struct, sys, logging, time
from aiosendspin.noise.keys import Identity
from aiosendspin.noise.trust_store import InMemoryServerPairingStore
from aiosendspin.server.server import SendspinServer
from aiosendspin.server.audio import AudioFormat
from aiosendspin.models.types import ConnectionReason

logging.basicConfig(level=logging.INFO)
URL = sys.argv[1] if len(sys.argv) > 1 else "ws://localhost:8928/sendspin"
SECS = float(sys.argv[2]) if len(sys.argv) > 2 else 8.0
RATE = 48000

def tone(start_frame: int, frames: int) -> bytes:
    out = bytearray()
    for i in range(start_frame, start_frame + frames):
        t = i / RATE
        # 440 Hz plus a 2 Hz amplitude wobble so continuity gaps are detectable
        v = 0.5 * math.sin(2 * math.pi * 440 * t) * (0.6 + 0.4 * math.sin(2 * math.pi * 2 * t))
        s = int(v * 32767)
        out += struct.pack("<hh", s, s)
    return bytes(out)

class PerfClock:
    """Windows time.monotonic() ticks at 15.6ms; QPC does not."""
    def now_us(self) -> int:
        return time.perf_counter_ns() // 1000

async def main():
    loop = asyncio.get_running_loop()
    server = SendspinServer(loop, Identity.generate(), "InteropServer",
                            pairing_store=InMemoryServerPairingStore(), clock=PerfClock())
    ids = []
    server.add_event_listener(lambda srv, ev: (print("EVENT", type(ev).__name__, getattr(ev, "client_id", None), flush=True), ids.append(getattr(ev, "client_id", None))))
    await server.connect_to_client_and_wait(URL, connection_reason=ConnectionReason.PLAYBACK,
                                            retry_initial_connection=True)
    print("connected", flush=True)
    await asyncio.sleep(1.0)
    cid = next((i for i in ids if i), None)
    assert cid, "no client id seen"
    print("client id", cid, flush=True)
    await server.trust_unpaired(cid)
    await asyncio.sleep(1.5)
    client = server.get_client(cid)
    print("client", client, flush=True)
    group = client.group
    stream = group.start_stream()
    frame = 0
    chunk = RATE // 5  # 200 ms of source audio per commit
    end = loop.time() + SECS
    while loop.time() < end:
        stream.prepare_audio(tone(frame, chunk), AudioFormat(sample_rate=RATE, bit_depth=16, channels=2))
        await stream.commit_audio()
        frame += chunk
        await stream.sleep_to_limit_buffer(2_000_000)
    print("done feeding", frame / RATE, "s", flush=True)
    await asyncio.sleep(1.0)
    group.stop_stream()
    await asyncio.sleep(0.5)
    await server.close() if hasattr(server, "close") else None

asyncio.run(main())
