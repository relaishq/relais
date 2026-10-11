# Caller memory findings for #41

The investigation found allocation churn and reclaimable runtime pages in the
caller harness, with no unbounded packet history or sustained stack growth.
This supports judging caller leaks by retained live memory after GC. Callers
are test tooling; product RSS remains a capacity constraint.

Hosted run 38159522631 had 611 samples per caller. After warm-up, every
caller's Go runtime reservation stayed exactly **22,640,904 bytes**. RSS grew
at 4.30, 6.11 and 4.49 KiB/s, while heap-in-use grew at 1.21, 0.83 and
0.98 KiB/s. Caller 5478 failed the former RSS gate at 21.5 MiB/hour. Those
samples do not contain the GC live-heap metric and cannot establish a pass
under the revised caller rule.

Local measurements used Go 1.26.9 on macOS, six calls across three generators,
two calls per generator, GOMAXPROCS=1 and the VP8 fixture. Early and late
allocation profiles showed about **523 MiB allocated over three minutes**
against about **0.5 MiB of live-profile change** in one caller. Main allocation
sites were frame assembly, bounded history copies, RTP reads and decoder
images. Each call retained roughly 0.38 MiB of payload history. Runtime
reservations stayed flat, stacks stayed within 0.69–0.88 MiB, and goroutine
counts stayed within 74–81.

An explicit diagnostic `debug.FreeOSMemory` probe released about **2.1 MiB**
of Go heap pages per caller. Heap objects after collection were near **3 MiB**.
Two callers' physical footprints fell by about 1.4–1.7 MiB while macOS RSS
barely changed. The temporary release probe was removed. Normal sampling
neither forces GC nor releases memory. Default heap profiles are sampled and
can lag two GC cycles; their small differences are not exact live-set counts.

Keyframes decode in pure Go. Interframe checks launch a separate ffmpeg
process, whose decoder memory is not included in the caller PID's RSS. The
precise source of hosted Linux RSS growth was not isolated by these Mac
experiments, and no harness leak fix is claimed.

Caller memory now gates on `/gc/heap/live:bytes`, the live objects marked by
the previous GC. RSS, physical footprint and heap-in-use remain diagnostic
trends. Product process-memory and heap gates are unchanged. See
[the soak memory contract](soak.md#flat-memory) for observation windows,
thresholds and acceptance requirements.
