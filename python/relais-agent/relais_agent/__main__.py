"""Listen on loopback; the worker connects. Run with --help for fixture options."""
import argparse
import asyncio
import base64
import hashlib
import hmac
import os
import ipaddress
import json
import struct
import time
from http import HTTPStatus

import av
from websockets.asyncio.server import serve

from .codec import Tone, decoder


def loopback(host):
    if not ipaddress.ip_address(host).is_loopback:
        raise ValueError("development bot requires a literal loopback address")
    return host


async def bot(ws, packets):
    await ws.send(json.dumps({"type": "hello", "protocol": 1, "bot_instance": "loopback"}))
    attached = json.loads(await ws.recv())
    if attached.get("protocol") != 1 or attached.get("resume") != "fresh":
        raise ValueError("unsupported development attachment")
    queue = asyncio.Queue(maxsize=32)
    started, begun, finished = asyncio.Event(), asyncio.Event(), asyncio.Event()
    credit_changed = asyncio.Condition()
    credit = {"revision": 0, "through_seq": 0, "packet_slots": 0, "payload_bytes": 0}
    pending = {}
    decode = decoder()
    input_samples = 0

    async def receive():
        nonlocal input_samples
        async for raw in ws:
            if isinstance(raw, bytes):
                if len(raw) < 19 or raw[0] != 1 or len(raw) > 18 + 1200:
                    raise ValueError("invalid input frame")
                input_samples += sum(f.samples for f in decode.decode(av.Packet(raw[18:])))
                started.set()
                continue
            message = json.loads(raw)
            kind, value = message["type"], message
            if kind == "output_credit":
                async with credit_changed:
                    if value["revision"] > credit["revision"]:
                        credit.update(value)
                        for seq in list(pending):
                            if seq <= credit["through_seq"]:
                                del pending[seq]
                        credit_changed.notify_all()
            elif kind == "output_begun":
                begun.set()
            elif kind == "output_receipt":
                if value["outcome"] != "admitted":
                    raise ValueError(f"output refused: {value['outcome']}")
            elif kind == "playback_finished":
                print(json.dumps({"playback": value, "input_samples": input_samples}), flush=True)
                finished.set()
            elif kind == "checkpointed":
                print(json.dumps({"checkpointed": message}), flush=True)
            elif kind in ("input_gap", "notice_gap", "refused"):
                raise ValueError(f"unexpected {kind}")
            elif kind == "ping":
                await ws.send(json.dumps({"type": "pong", "nonce": message["nonce"]}))

    async def produce():
        await started.wait()
        tone = Tone()
        origin = asyncio.get_running_loop().time()
        for seq in range(1, packets + 1):
            await asyncio.sleep(max(0, origin + (seq - 1) * .020 - asyncio.get_running_loop().time()))
            packet = tone.packet()
            ready = time.time_ns()  # bytes exist before queue and credit waits
            await queue.put((seq, packet))
            print(json.dumps({"seq": seq, "ready_ns": ready, "digest": hashlib.sha256(packet).hexdigest()}), flush=True)
        await queue.put(None)

    async def output():
        await started.wait()
        await ws.send(json.dumps({"type": "output_begin", "request_id": "tone", "turn": 1}))
        await begun.wait()
        while (item := await queue.get()) is not None:
            seq, packet = item
            async with credit_changed:
                await credit_changed.wait_for(lambda: credit["packet_slots"] > len(pending)
                                              and credit["payload_bytes"] >= sum(pending.values()) + len(packet))
                pending[seq] = len(packet)
            await ws.send(struct.pack(">BQQ", 2, 1, seq) + packet)
        await ws.send(json.dumps({"type": "audio_end", "turn": 1, "last_seq": packets}))
        await finished.wait()
        # This fixture has no conversation. Exercise the local state/ack path.
        await ws.send(json.dumps({"type": "checkpoint", "seq": 1, "version": 1,
                                  "working": base64.b64encode(b"tone complete").decode()}))
        await ws.send(json.dumps({"type": "replay_horizon", "seq": packets}))

    async with asyncio.TaskGroup() as tasks:
        tasks.create_task(receive())
        tasks.create_task(produce())
        tasks.create_task(output())


def authenticate(token):
    def check(connection, request):
        if request.path != "/bridge" or request.headers.get("Origin", ""):
            return connection.respond(HTTPStatus.FORBIDDEN, "loopback worker only\n")
        if not hmac.compare_digest(request.headers.get("Authorization", "").encode(), ("Bearer " + token).encode()):
            return connection.respond(HTTPStatus.UNAUTHORIZED, "development token required\n")
    return check


async def main(args):
    loopback(args.host)
    if args.token is None or not 16 <= len(args.token) <= 128 or any(ord(c) < 33 or ord(c) > 126 for c in args.token):
        raise ValueError("development token must be 16–128 visible ASCII bytes")
    if not 1 <= args.packets <= 3000:
        raise ValueError("packets must be 1–3000")

    async with serve(lambda ws: bot(ws, args.packets), args.host, args.port,
                     process_request=authenticate(args.token), max_size=32768, max_queue=1,
                     write_limit=32768, compression=None, ping_interval=None) as server:
        host = f"[{args.host}]" if ":" in args.host else args.host
        print(json.dumps({"url": f"ws://{host}:{server.sockets[0].getsockname()[1]}/bridge"}), flush=True)
        await server.serve_forever()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--token", default=os.environ.get("RELAIS_BRIDGE_DEV_TOKEN"),
                        help="development token (prefer RELAIS_BRIDGE_DEV_TOKEN)")
    parser.add_argument("--packets", type=int, default=150)
    asyncio.run(main(parser.parse_args()))
