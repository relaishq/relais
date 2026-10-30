"""Loopback runner: register with each --worker, then attach to the owner."""
import argparse
import asyncio
import base64
import hashlib
import ipaddress
import json
import os
import struct
import time
import uuid
from urllib.parse import urlsplit

import av
from websockets.asyncio.client import connect

from .codec import Tone, decoder


def loopback(host):
    if not ipaddress.ip_address(host).is_loopback:
        raise ValueError("development runner requires a literal loopback address")
    return host


def endpoint(value):
    url = urlsplit(value)
    loopback(url.hostname)
    if (url.scheme != "ws" or url.port is None or url.path != "/bridge"
            or url.username is not None or url.password is not None or url.query or url.fragment):
        raise ValueError("use ws://<literal-loopback>:<port>/bridge")
    return value


class NoRedirectConnect(connect):
    def process_redirect(self, exc):
        return exc


def socket(url, token):
    return NoRedirectConnect(endpoint(url.removesuffix("/registry")) + ("/registry" if url.endswith("/registry") else ""),
                   additional_headers={"Authorization": "Bearer " + token}, proxy=None,
                   max_size=32768, max_queue=1, write_limit=32768,
                   compression=None, ping_interval=None)


class Session:
    def __init__(self, packets):
        self.packets = packets
        self.queue = asyncio.Queue(maxsize=32)
        self.started = asyncio.Event()
        self.finished = asyncio.Event()
        self.pending = {}
        self.last = 0
        self.worker_task = None
        self.producer = None
        self.input_samples = 0
        self.decode = decoder()

    async def produce(self):
        await self.started.wait()
        tone = Tone()
        origin = asyncio.get_running_loop().time()
        for seq in range(1, self.packets + 1):
            await asyncio.sleep(max(0, origin + (seq - 1) * .020 - asyncio.get_running_loop().time()))
            packet = tone.packet()
            ready = time.time_ns()
            await self.queue.put((seq, packet))
            print(json.dumps({"seq": seq, "ready_ns": ready,
                              "digest": hashlib.sha256(packet).hexdigest()}), flush=True)
        await self.queue.put(None)

    async def attach(self, url, token, assignment):
        if self.worker_task is not None:
            self.worker_task.cancel()
            await asyncio.gather(self.worker_task, return_exceptions=True)
        ws = await socket(url, token)
        try:
            await ws.send(json.dumps({"type": "attach", "protocol": 1,
                                     **{k: assignment[k] for k in ("session_id", "epoch", "bot_instance", "generation", "attach_token")},
                                     "last_checkpoint_seq": 0, "last_output_seq": self.last}))
            attached = json.loads(await ws.recv())
            scope = attached["scope"]
            if (attached["type"] != "attached" or attached["protocol"] != 1
                    or any(scope[k] != assignment[k] for k in ("session_id", "epoch", "bot_instance", "generation", "lineage_id"))):
                raise ValueError("mismatched attachment")
            if attached["resume"] not in ("fresh", "planned_move"):
                raise ValueError("unsupported recovery")
            print(json.dumps({"attached_epoch": scope["epoch"], "pid": os.getpid()}), flush=True)
            next_seq = attached["next_output_seq"]
            self.pending = {s: p for s, p in self.pending.items() if s >= next_seq}
            self.worker_task = asyncio.create_task(self.serve(ws))
            if self.producer is None:
                self.producer = asyncio.create_task(self.produce())
        except BaseException:
            await ws.close()
            raise

    async def serve(self, ws):
        credit_changed = asyncio.Condition()
        begun = asyncio.Event()
        credit = {"revision": 0, "through_seq": 0, "packet_slots": 0, "payload_bytes": 0}

        async def receive():
            async for raw in ws:
                if isinstance(raw, bytes):
                    if len(raw) < 19 or raw[0] != 1 or len(raw) > 1218:
                        raise ValueError("invalid input frame")
                    self.input_samples += sum(f.samples for f in self.decode.decode(av.Packet(raw[18:])))
                    self.started.set()
                    continue
                message = json.loads(raw)
                kind = message["type"]
                if kind == "output_credit":
                    async with credit_changed:
                        if message["revision"] > credit["revision"]:
                            credit.update(message)
                            self.pending = {s: p for s, p in self.pending.items() if s > credit["through_seq"]}
                            credit_changed.notify_all()
                elif kind == "output_begun":
                    begun.set()
                elif kind == "output_receipt" and message["outcome"] != "admitted":
                    raise ValueError(f"output refused: {message['outcome']}")
                elif kind == "playback_finished":
                    print(json.dumps({"playback": message, "input_samples": self.input_samples}), flush=True)
                    self.finished.set()
                elif kind == "checkpointed":
                    print(json.dumps({"checkpointed": message}), flush=True)
                elif kind in ("input_gap", "notice_gap", "refused"):
                    raise ValueError(f"unexpected {kind}")
                elif kind == "ping":
                    await ws.send(json.dumps({"type": "pong", "nonce": message["nonce"]}))
            # Socket loss cancels output; the runner must obtain a new assignment.
            raise ConnectionError("owner socket closed")

        async def send_packet(seq, packet):
            self.last = max(self.last, seq)
            self.pending[seq] = packet
            async with credit_changed:
                await credit_changed.wait_for(lambda: credit["packet_slots"] >= len(self.pending)
                                              and credit["payload_bytes"] >= sum(map(len, self.pending.values())))
            await ws.send(struct.pack(">BQQ", 2, 1, seq) + packet)

        async def output():
            await self.started.wait()
            await ws.send(json.dumps({"type": "output_begin", "request_id": "tone", "turn": 1}))
            await begun.wait()
            # Exact host cut proves which identities never reached admission.
            for seq, packet in list(self.pending.items()):
                await send_packet(seq, packet)
            while (item := await self.queue.get()) is not None:
                await send_packet(*item)
            await ws.send(json.dumps({"type": "audio_end", "turn": 1, "last_seq": self.packets}))
            await self.finished.wait()
            await ws.send(json.dumps({"type": "checkpoint", "seq": 1, "version": 1,
                                      "working": base64.b64encode(b"tone complete").decode()}))
            await ws.send(json.dumps({"type": "replay_horizon", "seq": self.packets}))

        try:
            async with asyncio.TaskGroup() as tasks:
                tasks.create_task(receive())
                tasks.create_task(output())
        finally:
            await ws.close()

    async def close(self):
        tasks = [t for t in (self.worker_task, self.producer) if t is not None]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)


class Runner:
    def __init__(self, token, packets):
        self.token, self.packets = token, packets
        self.runner_id = "runner-" + uuid.uuid4().hex
        self.bot_instance = "bot-" + uuid.uuid4().hex
        self.floors = {}
        self.sessions = {}
        self.reservations = {}

    def accept(self, assignment):
        session = assignment["session_id"]
        pair = assignment["epoch"], assignment["generation"]
        identity = assignment["bot_instance"], assignment["lineage_id"]
        previous = self.floors.get(session)
        if previous and (pair < previous[0] or identity != previous[1] or pair[1] != previous[0][1]):
            raise ValueError("stale assignment or changed lineage; replacement requires revocation")
        self.floors[session] = pair, identity

    async def registry(self, url, ready):
        async with socket(url + "/registry", self.token) as ws:
            await ws.send(json.dumps({"type": "hello", "protocol": 1, "runner_id": self.runner_id,
                                      "bot_instance": self.bot_instance, "agent_name": "bridge",
                                      "config_revision": "1", "capacity": 1}))
            reserved = None
            async for raw in ws:
                message = json.loads(raw)
                kind = message["type"]
                if kind == "registered":
                    ready.set()
                elif kind == "reserve":
                    session = message["session_id"]
                    previous = self.floors.get(session)
                    now = time.time_ns() // 1000000
                    self.reservations = {worker: r for worker, r in self.reservations.items() if r["expires_ms"] > now}
                    accepted = ((not self.sessions or session in self.sessions)
                                and all(r["session_id"] == session for r in self.reservations.values())
                                and message["expires_ms"] > now
                                and (previous is None or message["epoch"] >= previous[0][0]))
                    reserved = message if accepted else None
                    if accepted:
                        self.reservations[url] = message
                    await ws.send(json.dumps({"type": "reserved", "reservation_id": message["reservation_id"], "accepted": accepted}))
                elif kind == "assign":
                    if (reserved is None or self.reservations.get(url) != reserved or any(message[k] != reserved[k] for k in ("session_id", "epoch", "expires_ms"))
                            or time.time_ns() // 1000000 >= message["expires_ms"]):
                        raise ValueError("unreserved or expired assignment")
                    reserved = None
                    del self.reservations[url]
                    self.accept(message)
                    session = self.sessions.setdefault(message["session_id"], Session(self.packets))
                    await session.attach(url, self.token, message)
                else:
                    raise ValueError("unsupported registry message")


async def main(args):
    if args.token is None or not 16 <= len(args.token) <= 128 or any(ord(c) < 33 or ord(c) > 126 for c in args.token):
        raise ValueError("development token must be 16–128 visible ASCII bytes")
    if not 1 <= args.packets <= 3000:
        raise ValueError("packets must be 1–3000")
    runner = Runner(args.token, args.packets)
    try:
        async with asyncio.TaskGroup() as tasks:
            ready = [asyncio.Event() for _ in args.worker]
            for url, event in zip(args.worker, ready):
                tasks.create_task(runner.registry(endpoint(url), event))
            for event in ready:
                await event.wait()
            print(json.dumps({"ready": True, "pid": os.getpid()}), flush=True)
    finally:
        await asyncio.gather(*(session.close() for session in runner.sessions.values()))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worker", action="append", required=True)
    parser.add_argument("--token", default=os.environ.get("RELAIS_BRIDGE_DEV_TOKEN"))
    parser.add_argument("--packets", type=int, default=150)
    asyncio.run(main(parser.parse_args()))
