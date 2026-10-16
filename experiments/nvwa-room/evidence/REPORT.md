# Nvwa 房间 spike：25d8e35 最终配对验收

日期：2026-10-16。生产基线 `25d8e35a7e0edf53e14bbe3cfc531820964282a1`，包含 W2 审查修复及 W1 DataChannel hardening。本轮只改实验驱动、测试探针与浏览器页面，没有修改生产包、agent v2 或 task；未 commit、push、发 PR 或写 GitHub。主24轮使用同一组固定二进制；burst接收探针随后单独修复，重跑两种burst，其room二进制另存hash，生产Relais二进制始终不变。SHA-256 和源文件清单位于 `stage3/`。

**结论：GO（受限本地实验）；整体PARTIAL，公网/正式采用HOLD** 只针对本机、受信的两个客户端、小消息、房间进程存活、允许显式新连接的模型。此结果不证明公网/全球互通、正式 Nvwa 接入或生产容量。

历史原件保留：[stage-1](REPORT.stage1.md)、[stage-2 与审查修复](REPORT.stage2-review-fixes.md)，原始 `stage2/`、`review-fixes/` 均未覆盖。

## 1. 精确启动、运行、停止与清理（VERIFIED）

从仓库根目录执行。模块只用本地缓存，禁止新增下载；动态 loopback 端口排除6379、9101、9201。

```sh
nvwa_scratch=$(mktemp -d /private/tmp/nvwa-room-stage3.XXXXXX)
bash experiments/nvwa-room/scripts/build.sh "$nvwa_scratch"

# WS应用数据 + Relais音频；启动器自建 Redis/relay/control/signal/两worker/room。
"$nvwa_scratch/bin/topology" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/manual-ws" > "$nvwa_scratch/manual-ws.log" 2>&1 &
nvwa_topology_pid=$!
# manual-ws/topology.json 中的 room_url/web/ 是浏览器页面。
# 停止时只使用本次保存的PID；启动器逐个等待自己启动的子进程。
kill -TERM "$nvwa_topology_pid"
wait "$nvwa_topology_pid"

# DC与音频共享PC；其余拓扑相同。
"$nvwa_scratch/bin/topology" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/manual-dc" -datachannel > "$nvwa_scratch/manual-dc.log" 2>&1 &
nvwa_topology_pid=$!
# 同样 kill -TERM "$nvwa_topology_pid"; wait "$nvwa_topology_pid"

# 每次 -out 必须为新的任务目录；driver 退出会清理自己的拓扑。
"$nvwa_scratch/bin/driver" -bin "$nvwa_scratch/bin" -out "$nvwa_scratch/one-dc" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg" \
 -count 1000 -rate 30 -fault disconnect -datachannel
# fault可为normal/disconnect/worker-kill/lost-receipt；去掉-datachannel即WS。

# 两个完整回归门：unit直接跑；长harness使用wefty。
python3 experiments/nvwa-room/scripts/regression-stage2.py \
 --out "$nvwa_scratch/unit" --gate test-unit
wefty exec --detach --json --max-runtime 20m -- \
 python3 experiments/nvwa-room/scripts/regression-stage2.py \
 --out "$nvwa_scratch/harness" --gate test-harness-short
# 此wrapper保留make真实非零状态。unit脚本会启动并回收专用非6379 Redis。

# 待两个门结束后，交替运行24次配对矩阵和短aux试验。
wefty exec --detach --json --max-runtime 35m -- \
 python3 experiments/nvwa-room/scripts/run-stage3.py --scratch "$nvwa_scratch"
# 不需回归等待的独立复跑：
# python3 experiments/nvwa-room/scripts/run-matrix.py --scratch "$nvwa_scratch" --transport paired
wefty inspect RUN_ID --json
wefty logs RUN_ID --follow
# 只中止本任务的run时：wefty cancel RUN_ID；不按进程名/端口杀进程。

# 单独复跑aux；参数相同，每次使用新out。
"$nvwa_scratch/bin/burst" -bin "$nvwa_scratch/bin" -out "$nvwa_scratch/burst-dc-manual" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg" -datachannel
"$nvwa_scratch/bin/backpressure" -bin "$nvwa_scratch/bin" -out "$nvwa_scratch/pressure-dc-manual" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg" -datachannel
"$nvwa_scratch/bin/policies" -bin "$nvwa_scratch/bin" -out "$nvwa_scratch/policies-dc-manual" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg"
# burst/backpressure去掉-datachannel即可WS对照。
```

worker 配置为 `-agent=room-audio -room-audio-bus=udp://127.0.0.1:PORT -frame-cache-off`。DC另开 `-datachannel -datachannel-upstream=ws://127.0.0.1:ROOM/relais-upstream`，默认1 KiB消息、16 KiB pending/channel、4次channel admission。Redis、TLS证书和bearer均由任务启动器独立创建；没有使用用户Redis或浏览器。保留scratch供审查；清理由各启动器的子进程句柄与Wait完成，不扫描全机PID，也不删除共享状态。

## 2. 构建、启动、功能、故障、音频与测量

| 范围 | 状态 | 实际证据与边界 |
| --- | --- | --- |
| 构建、vet、全模块lint | VERIFIED | golangci-lint v2.14.0运行`./...`，无目录排除；stock worker依赖与符号不含experiments包。internal opt-in音频实现仍链接。 |
| 实际启动 | VERIFIED | 每轮独立Redis/relay/两worker/control/TLS+bearer signal/room；两个不同session和服务器绑定玩家。 |
| 房间功能 | VERIFIED | join、wave v1/start_tick、stop、snapshot、query；同一Room.Handle和epoch/player/command_id账本。原生角色/动画不在范围内。 |
| 在途故障恢复 | VERIFIED（新连接） | 真Send后、房间提交后、回执前切断；new signal session、同token、generation1→2、query/retry一致、去重一次、副作用一次。恢复由脚本主动触发，非自主检测延迟。 |
| 丢回执 | VERIFIED | 房间已应用并记账后丢一次应用收据；query/retry返回原结果；变化input_seq显式冲突。 |
| 双向音频 | VERIFIED（合成音调） | Go匹配对方的互斥Opus参考包；浏览器FFT识别对方440/880 Hz，非自身echo。不代表真人音质。 |
| 测量 | VERIFIED（本机样本） | 同机逐操作RTT、状态年龄、实际收据journal、队列字节/年龄、重连及音频包间隔；全栈RSS和音频源端延迟未测。 |
| 默认媒体回归 | PARTIAL | 两个完整make门均实际运行，失败原文保留。 |

身份由无登录的本地lobby发放有限token；服务器不接受自报player_id。连接有服务端generation，旧订阅停止、旧代次更新拒绝；generation不进入去重键。成功表示房间已经应用且记录结果，非Write/SCTP ACK。state仅采用更新序号，重连不补播旧高频输入。

**非本地安全限制（PARTIAL）：** `/lobby`无认证；`/relais-upstream`从query string接收session ID，不独立认证发送者；token不轮换；最新join获胜，没有compare-and-set。30分钟随机token、Origin和loopback限制不能替代公网身份/会话安全。

**房间重启（NOT-RUN）：** 新构造产生新epoch，旧epoch返回显式`epoch_reset`仅有单元测试；本轮没有真实room进程重启试验，内存账本也不提供崩溃持久恢复。

## 3. 同条件配对原始结果（VERIFIED）

固定每人1000编号操作：500 trigger/stop可靠命令、500 state，30 Hz；payload固定；Opus20 ms/32 kb/s，A440 Hz/B880 Hz。DC reliable ordered control与unordered/maxRetransmits=0 state共用音频PC；WS只替换应用适配，音频仍走同一Relais路径。每对交替谁先运行，同一组固定二进制。自身完整回归门结束后开始矩阵；首条WS正常试验短暂与一个功能性burst预检重叠，原记录未删除。未做全机CPU/网络隔离，不能据微小本机延迟差异归因或外推公网。

RTT包含故障目标命令的恢复/query/retry时间。状态年龄是接收快照时最后状态的同机墙钟年龄，包含离线期间变老；不是跨机时钟测量。队列列为房间输出含在写普通消息，另预留终止错误帧。音频中断为编码包最大到达间隔，不是主观听感或端到端音频延迟。

| Run | RTT mean A/B ms | RTT max A/B ms | State age max A/B ms | Queue B / age ms | Recovery A ms | Audio gap A/B ms | Version |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| normal-1-ws | 0.494/0.494 | 7.556/7.600 | 74.698/74.021 | 952/1.527 | 0.000 | 41.009/39.020 | 2002 |
| normal-1-dc | 1.398/1.388 | 11.876/13.858 | 75.492/75.757 | 970/2.943 | 0.000 | 35.197/30.692 | 2002 |
| normal-2-ws | 0.594/0.589 | 8.342/8.300 | 79.663/79.770 | 952/3.093 | 0.000 | 43.572/59.419 | 2002 |
| normal-2-dc | 1.440/1.331 | 46.163/20.041 | 90.584/75.481 | 970/3.484 | 0.000 | 52.848/65.461 | 2002 |
| normal-3-ws | 0.575/0.646 | 9.667/27.290 | 89.184/94.874 | 952/2.962 | 0.000 | 54.607/64.021 | 2002 |
| normal-3-dc | 1.421/1.409 | 14.478/12.491 | 82.007/78.679 | 970/1.219 | 0.000 | 39.717/31.745 | 2002 |
| disconnect-1-ws | 0.768/0.364 | 263.537/2.964 | 299.681/295.773 | 894/1.345 | 295.368 | 485.099/399.657 | 2003 |
| disconnect-1-dc | 1.951/1.345 | 478.906/60.649 | 543.645/538.692 | 965/6.243 | 475.577 | 483.263/400.021 | 2003 |
| disconnect-2-ws | 0.948/0.687 | 280.000/235.029 | 333.264/319.743 | 940/7.932 | 279.537 | 496.667/419.107 | 2003 |
| disconnect-2-dc | 3.284/2.655 | 491.047/87.905 | 533.334/583.374 | 965/8.797 | 521.845 | 507.128/439.072 | 2003 |
| disconnect-3-ws | 0.462/0.184 | 258.013/7.243 | 301.071/291.352 | 894/2.511 | 257.691 | 491.946/420.031 | 2003 |
| disconnect-3-dc | 1.021/0.593 | 472.767/12.498 | 533.071/504.418 | 912/2.190 | 471.526 | 490.240/420.032 | 2003 |
| worker-kill-1-ws | 0.255/0.270 | 9.001/17.770 | 74.971/75.859 | 897/4.684 | 8.550 | 245.824/240.172 | 2003 |
| worker-kill-1-dc | 1.457/1.205 | 221.757/23.460 | 267.644/261.733 | 961/8.323 | 220.392 | 237.438/160.538 | 2003 |
| worker-kill-2-ws | 0.394/0.445 | 61.202/64.629 | 101.734/102.330 | 897/3.155 | 5.733 | 239.982/240.040 | 2003 |
| worker-kill-2-dc | 1.382/1.122 | 227.049/35.323 | 267.022/259.989 | 965/12.283 | 259.036 | 248.366/180.158 | 2003 |
| worker-kill-3-ws | 0.621/0.521 | 50.962/26.158 | 98.296/108.232 | 943/1.531 | 7.158 | 242.453/239.854 | 2003 |
| worker-kill-3-dc | 2.131/1.903 | 218.800/58.155 | 267.750/251.383 | 915/11.989 | 250.771 | 249.691/165.654 | 2003 |
| lost-receipt-1-ws | 0.793/0.576 | 193.738/18.300 | 225.946/93.019 | 952/4.149 | 0.000 | 71.495/64.495 | 2002 |
| lost-receipt-1-dc | 1.691/1.533 | 151.879/36.251 | 167.150/98.287 | 970/1.344 | 0.000 | 74.470/84.459 | 2002 |
| lost-receipt-2-ws | 0.646/0.515 | 151.184/38.336 | 172.293/89.748 | 952/3.436 | 0.000 | 81.671/72.577 | 2002 |
| lost-receipt-2-dc | 1.066/0.894 | 152.742/8.061 | 168.107/81.248 | 970/0.702 | 0.000 | 27.747/29.972 | 2002 |
| lost-receipt-3-ws | 0.812/0.689 | 151.215/47.828 | 166.230/85.787 | 952/4.758 | 0.000 | 100.460/136.365 | 2002 |
| lost-receipt-3-dc | 1.693/1.589 | 152.583/124.782 | 192.486/215.142 | 970/1.985 | 0.000 | 168.971/170.238 | 2002 |

24轮均每人1000操作完成、每轮1000次总命令应用、0额外命令应用、两端最终state_seq=1000且房间版本一致。12次在途cut均generation1→2/dedup=1/目标副作用=1；6次lost-receipt均drop=1/dedup=1/conflict=1。共48,000编号操作、80,160个peer参考包，self/unknown均0；独立验证480个实收Opus样本属于对方互斥参考集合。每轮missing=0/0，实际收据journal重复/乱序=0/0；但每客户端基本串行等待应用回执，后两者主要受驱动构造约束，不是主动乱序/复制网络验收。

### 在途断连与worker SIGKILL

每次目标命令p1-0250：`RequestWithSent`在实际传输Send完成后发信号；房间只在测试模式中暂停真实writer回执，最长2秒。只读metrics屏障确认命令已应用、原请求仍等待，再执行cut。query与retry均在新generation，结果和原结果版本必须相同。断连不是丢回执故障替身；该切点固定提交后，不覆盖提交前的不确定结果分支。

| 运行 | Send→cut ms | cut→恢复 ms | generation | dedup / 目标副作用 | 原结果版本 |
| --- | ---: | ---: | --- | ---: | ---: |
| disconnect-1-ws | 0.805 | 262.376 | 1→2 | 1/1 | 503 |
| disconnect-1-dc | 2.702 | 474.272 | 1→2 | 1/1 | 504 |
| disconnect-2-ws | 4.651 | 274.431 | 1→2 | 1/1 | 504 |
| disconnect-2-dc | 6.358 | 483.500 | 1→2 | 1/1 | 503 |
| disconnect-3-ws | 2.520 | 255.267 | 1→2 | 1/1 | 504 |
| disconnect-3-dc | 2.207 | 469.805 | 1→2 | 1/1 | 504 |
| worker-kill-1-ws | 1.588 | 7.189 | 1→2 | 1/1 | 504 |
| worker-kill-1-dc | 7.326 | 213.583 | 1→2 | 1/1 | 504 |
| worker-kill-2-ws | 1.017 | 4.931 | 1→2 | 1/1 | 504 |
| worker-kill-2-dc | 4.955 | 221.412 | 1→2 | 1/1 | 503 |
| worker-kill-3-ws | 0.723 | 6.636 | 1→2 | 1/1 | 504 |
| worker-kill-3-dc | 1.794 | 215.943 | 1→2 | 1/1 | 503 |

worker-kill只SIGKILL本轮topology记录的worker-1，room/Redis存活。**DC与音频共享PC，worker丢失会中断音频。** WS应用本来独立于worker，脚本主动关闭WS并重建以对齐恢复合同；这不是WS必须跟随worker故障重连的证据。

### Burst、背压、Move和Drain

Burst是**传输诊断**：每人向有界测试echo lane瞬发200个4字节编号消息，无逐条应用回执等待；正常房间120输入/秒合同不变，未把限速关闭后冒充正常应用容量。DC使用同一PC第三条可靠burst channel，经真实worker/op0上游；WS用同一room的`/test-burst`。仅`-test-faults`启用，每连接最多200条，普通16 KiB/128条输出队列界限保持。正常应用snapshot与两路peer音频必须在burst后继续。最初DC burst通过、WS burst失败；WS驱动收到非4字节回复，同类race复现明确slow_consumer，暴露探针可先积压到128条。修复仅让测试echo等待真实writer完成再读取下一条，实施有界接收背压；客户端仍一次发200条、不逐条等回执，普通房间限速/队列未变。两种burst随后均用同一修复后的room探针重跑；原始失败和两版hash保留，不混充24轮矩阵的二进制。它不证明正常命令可无限突发，也不证明部分可靠通道的拥塞收益。

| 传输 | 发送/实收 A、B | 瞬发耗时 A/B ms | RTT均值 A/B ms | RTT最大 A/B ms | peer包前→后 A、B | 音频间隔 A/B ms |
| --- | --- | ---: | ---: | ---: | --- | ---: |
| dc | 200/200, 200/200 | 1.527/1.807 | 3.957/5.281 | 4.626/6.443 | 20→40, 20→40 | 27.919/29.929 |
| ws | 200/200, 200/200 | 1.580/2.881 | 1.743/1.685 | 2.174/2.579 | 20→40, 20→41 | 21.155/21.197 |

| 传输 | 全部应用 | 队列峰值 B / 年龄 ms | typed slow_consumer | peer包 A/B | 音频间隔 A/B ms |
| --- | ---: | ---: | --- | ---: | ---: |
| dc | 80 | 15026/519.241 | False | 83/83 | 22.945/22.526 |
| ws | 80 | 15350/528.303 | True | 71/72 | 21.663/23.765 |

**背压界限/媒体继续 VERIFIED；DC客户端typed原因送达 PARTIAL。** 两端producer命令全部应用，room各记录一次slow_consumer。WS客户端实际读到typed slow_consumer；DC只见datachannel_closed，未读到typed应用帧。驱动原接受条件允许数据通道关闭且room原因明确，导出器额外保留了这一差别，未把退出0误写成typed帧送达。关闭前最后错误帧是best effort；若产品要求客户端必达typed原因，需要后续通知/查询协议，不能靠可靠通道名称作保证。

Move实际返回 **HTTP 409 / data_session_not_movable**；原调用仍可snapshot，peer包由20增至35。Drain为 **HTTP 200 / ended**，room实收原session的op3/reconnect_required；新session ID不同，generation=2，恢复217.066 ms，跨session音频间隔229.546 ms。原命令query/retry一致，applied=1、dedup=1。不是原PC迁移。

原始结果、每条收据/屏障/音频到达时间、包样本和校验值见[stage3/summary.json](stage3/summary.json)、[stage3/manifest.json](stage3/manifest.json)、[stage3/auxiliary-summary.json](stage3/auxiliary-summary.json)、[stage3/auxiliary-manifest.json](stage3/auxiliary-manifest.json)。统计与原始数据由独立导出检查相互核对。

## 4. 浏览器双向音调（VERIFIED（本机Chromium））

```sh
wefty exec --detach --json --max-runtime 2m -- \
 python3 experiments/nvwa-room/scripts/browser-smoke.py --scratch "$nvwa_scratch" \
 --transport ws --audio-test-mode service-unsandboxed --attempt final
# 在同一scratch用--transport dc，即另一组独立拓扑、进程与profile。
```

每个进程均为任务自有 `/opt/homebrew/bin/chromium --headless=new --no-first-run --no-default-browser-check --disable-gpu --no-proxy-server --use-fake-device-for-media-stream --use-fake-ui-for-media-stream --use-file-for-fake-audio-capture=RUN_CWD/tone-HZ.wav --autoplay-policy=no-user-gesture-required --user-data-dir=TASK_SCRATCH/profile-HZ`，只加载本地`http://127.0.0.1:PORT/web/?auto=1&count=90&tone=HZ&transport=ws|dc`。最终成功运行额外使用 **`--disable-features=AudioServiceSandbox`**，这是局部测试设置，不是生产推荐。早期`--no-sandbox`尝试也仅作用于同一类隔离本地实例；失败记录完整保留。未操作Derek的Chrome或共享DevTools。

PCM fixture在wefty执行CWD中生成并验证：RIFF signed16-bit LE、mono、48 kHz、96000 frames、192044 B；不是调用方scratch路径缺失。窄标志消除了文件打开错误，但初次仍静音：输入media-source有能量、RTP包到达，AudioContext running，jitterBufferEmittedCount却为0。非零analyser gain试验无效；`--no-sandbox`也无效。实际修复是将远端track接到HTML audio sink并play，使Chrome开始拉取解码音频；sink音量1e-6避免测试声响，FFT仍取未衰减的远端track。此路径与[Chromium记录的WebAudio远端流限制](https://issues.chromium.org/issues/40094084)一致，不能用“收到RTP包”冒充解码后音调证据。

| 传输 | 玩家 / 输入→peer Hz | 有效peer样本 | peer高于自身频率 dB |
| --- | --- | ---: | ---: |
| ws | p2 / 880→440 | 67 | 60.339 |
| ws | p1 / 440→880 | 67 | 58.515 |
| dc | p1 / 440→880 | 64 | 58.700 |
| dc | p2 / 880→440 | 64 | 59.506 |

每人每传输90应用操作（45命令/45状态），无应用错误。采样窗口要求peer峰值>-50 dB、有效样本至少10个、peer相对own均值至少15 dB；实际值如上。两种方向都须满足，自己的echo不可能同时满足对方频率。完整FFT时间序列、RTC统计、原始命令行和失败尝试均保留。未检验浏览器故障矩阵、Safari/mobile或真人音质。

## 5. 缓冲、维护成本与能力边界（PARTIAL）

房间每消息<=1024 B、state payload<=128 B、pending<=16384 B含在写及终止预留、普通队列<=128条；最多两玩家/每人4096结果/16连接/120输入每秒，音频只保留两路最新<=1200 B包并100 ms过期。W1每channel应用队列16 KiB及SCTP发送payload16 KiB，最多4次生命周期admission；两预算各最多64 KiB/session，另有4×1024 B取出待写。读取scratch4×64 KiB、SCTP receive window64 KiB、observer TSN bitset8192 B/最多64 stream记录，库/内核/allocator开销另计。这是组件预算，**不是严格进程RSS上限**。

**分片超限已改策略，仍有边界：** 移除16-entry reassembly cap后合法小消息burst可继续。完整超限消息按通道策略拒绝/计数；非合规分片导致超限pending且无应用交付时，约2秒看门狗加200 ms reset宽限后关闭该session所有数据通道、停止本地SCTP，DTLS reader和媒体继续；不发会导致peer关闭共享DTLS的SCTP ABORT。SCTP接收窗口不是严格分片字节admission或RSS上限，不能宣称每个不可信peer都被硬限制为1 KiB内存。此行为由W1真实DATA/I-DATA和媒体回归提供证据，本W2轮没有另做恶意分片注入。第5条channel仅reset其自身；此项也沿用W1测试证据，不冒充本轮burst测试了第5条。

op3结束通知是50 ms预算的best effort；上游拥塞时可能丢失原因，不应依赖必达。data session禁止原通道迁移，Drain采用终止后重连。正常WS与DC应用合同相同，但WS可靠有序、DC state部分可靠的传输保证并不相同；本轮无注入公网丢包，不能证明DC状态时效更优。

复用了Relais TLS/bearer signal、控制面放置、真实relay/worker媒体、Redis所有权/代次与legacy agent。新增工作仍包括权威房间、lobby/token/generation、应用账本与冲突/状态协议、两客户端/recorder及跨session UDP音频bus；它不是现成多人SFU，不做混音、重定时或质量保障。WS应用路径更直接，故障域与音频分离；DC增加SCTP生命周期、worker→room hop、缓冲/看门狗和禁迁移策略，维护面更大。采用DC必须有复用PC或客户端能力的实际产品收益。

## 6. GO / HOLD / NO-GO 与未验证清单

- **GO：** 本机loopback、受信客户端、合法小消息、room持续存活、允许新signal会话恢复的应用原型；保留WS作为简单基线，DC可继续验证消费端复用PC的产品价值。Go与两浏览器均证明对方音调到达。
- **HOLD：** 正式Rust/Nvwa采用、公网发布、生产容量或安全承诺；需真实消费端/平台覆盖、认证与session绑定、持久恢复方案、跨机/TURN测试及可运行完整unit门的环境。全模块lint和本机harness绿灯不等于CI已绿。
- **NO-GO（本版本不承诺）：** 原DataChannel/DTLS/SCTP透明迁移或崩溃续接；不可信分片peer的严格1 KiB内存硬界；房间崩溃后持久命令恢复。若产品必须具备这些条件，本版本不能采用。

**NOT-RUN：** 原生Rust consumer和正式Nvwa native/Web接入、角色/骨骼资产播放、公网/TURN/TCP/TLS fallback、跨主机/跨区域/全球网络、Safari和mobile、真实room进程重启、房间崩溃持久恢复、负载容量/长期稳定性、原DataChannel/DTLS/SCTP迁移、全栈RSS/缓冲占用、真人音质。浏览器成功只代表这两个本机Chromium测试方向。

## 7. 测试、失败、审查与CI

| 检查 | 状态 | 墙钟 / 证据 |
| --- | --- | --- |
| make test-unit + 自建Redis | PARTIAL / FAIL | 258.644秒，exit=2；仅两个sysctl用例失败。 |
| make test-harness-short | VERIFIED / PASS | 642.251秒，exit=0。 |
| stock-worker build | VERIFIED / PASS | 真实go list deps + build + nm，排除experiments包/符号。 |
| golangci-lint v2.14.0 ./... | VERIFIED / PASS | 全模块0 issues，无目录排除。 |
| go vet ./...、JS/Python语法、diff check | VERIFIED / PASS | 本轮实际执行。 |
| 实验包与internal音频agent race/short/count10 | VERIFIED / PASS | 包含新测试lane opt-in及200条生命周期界限；早期race失败日志另存。 |
| 配对、burst、策略、浏览器 | VERIFIED / PASS | 本文原始数据及gzip收据；初始WS burst失败另存。 |
| 背压 | VERIFIED（界限/媒体） / PARTIAL（DC typed帧） | DC关闭和room slow_consumer计数已验证，客户端未收到typed应用帧。 |

unit门专用Redis `127.0.0.1:60938`，GOMAXPROCS=4、go test -p2，启动器已回收。仅`internal/udpbuffer/TestApplyReportsEffectiveSize`和`TestApplyReportsClampWithoutFailing`的`sysctl -n kern.ipc.maxsockbuf`在沙箱内exit1；未通过wefty绕过该已知限制，未修改断言/阈值。完整harness走wefty、memory/short；表中墙钟采用wrapper记录的make实际时间，不混用ledger等待时间。

本轮新增burst gate单测初版一次在race/count10遇到显式slow_consumer，因为把200个未读取回执直接压入既有128-entry队列。实际WS burst也暴露同类探针输出积压。测试echo后来增加writer完成背压，正常队列策略未放宽；单测恢复成200条先全发后全收，并在race/count10通过。独立cmd/burst两传输仍各实跑200瞬发，早期失败原文保留。

W1独立生产修复/红绿测试由编排器交付的`W1-REVIEW-FIXES.md`记录；本worker未重新执行全部W1恶意peer测试，也不声称独立审查已完成。此前主线5次定向对照及审查结论保留于历史报告。配对wefty bundle `run_5b5c53e01df3448865d722bd4a4594ab`最终为failed，原因是初版WS burst探针；其24轮矩阵阶段exit0，后续两种修正burst直接复跑均exit0。这些结果分开记录，未把bundle改写为成功。本轮没有触发hosted CI、没有GitHub写入或新模块/工具链下载；全模块lint/本地回归不等于CI已绿。所有wefty调用与结果均记入派工scratch的`wefty-exec-log.md`。
