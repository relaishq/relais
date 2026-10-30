"""Pinned PyAV Opus fixture codec; WebRTC and SRTP remain in the worker."""
import math
import struct

import av


class Tone:
    def __init__(self):
        self.encoder = av.CodecContext.create("libopus", "w")
        self.encoder.sample_rate = 48000
        self.encoder.layout = "stereo"
        self.encoder.format = "s16"
        self.encoder.bit_rate = 64000
        self.encoder.options = {"application": "voip", "frame_duration": "20"}
        self.samples = 0

    def packet(self):
        frame = av.AudioFrame(format="s16", layout="stereo", samples=960)
        frame.sample_rate = 48000
        frame.pts = self.samples
        samples = [int(8000 * math.sin(2 * math.pi * 660 * (self.samples + i) / 48000))
                   for i in range(960)]
        frame.planes[0].update(struct.pack("<1920h", *[s for s in samples for _ in range(2)]))
        self.samples += 960
        packets = self.encoder.encode(frame)
        if len(packets) != 1:
            raise RuntimeError("expected one 20 ms Opus packet")
        return bytes(packets[0])


def decoder():
    codec = av.CodecContext.create("opus", "r")
    codec.extradata = b"OpusHead" + struct.pack("<BBHIhB", 1, 2, 0, 48000, 0, 0)
    return codec
