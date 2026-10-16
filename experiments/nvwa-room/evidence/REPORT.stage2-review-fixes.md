# Nvwa 房间原型：DataChannel 与 WebSocket 配对验证

日期：2026-10-16。stage-2 历史基线：`spike/nvwa-room` / `866a07d02270312bddd9bb60a74185d953248997`。历史 stage-2 由编排器提交为 `705b3e2`。本次审查修复基于该提交，修改实验目录、`pkg/workerapp/agent.go` 的导入和新增 `internal/spike/roomaudio`；未修改 W1 的 `pkg/mediaworker`、`pkg/controlplane`、`pkg/workerapp/app.go`、agent v2 或 task 代码。本次修改未提交、push 或写入 GitHub。

**总状态：PARTIAL / HOLD。** 小消息、权威房间和“新连接恢复应用”路径有本机运行证据。浏览器消息已运行，但指定音调输入读取失败。严格入站分片边界仍为 W1 的 HOLD；完整默认媒体回归门也未全绿。不能据此宣布正式 Nvwa 接入或生产就绪。

## 1. 精确启动、停止与清理

所有命令从仓库根目录运行。使用新任务目录，已有缓存模块；`GOPROXY=off`、`-mod=readonly` 禁止新增模块下载。先完成构建，再固定二进制进行比较。

```sh
nvwa_scratch=$(mktemp -d /private/tmp/nvwa-room-stage2.XXXXXX)
bash experiments/nvwa-room/scripts/build.sh "$nvwa_scratch"

# WS 数据 + Relais 音频。启动器自建专用 Redis、relay、两名 worker、control、signal、房间。
"$nvwa_scratch/bin/topology" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/manual-ws" > "$nvwa_scratch/manual-ws.log" 2>&1 &
nvwa_topology_pid=$!
# 日志/topology.json 给出 room_url；页面为 ROOM_URL/web/。
# 只停止刚创建并保存 PID 的这个启动器；它逐个等待自己的子进程退出。
kill -TERM "$nvwa_topology_pid"
wait "$nvwa_topology_pid"

# DC 与音频共享 PeerConnection。房间上游由启动器写入 worker 配置。
"$nvwa_scratch/bin/topology" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/manual-dc" -datachannel > "$nvwa_scratch/manual-dc.log" 2>&1 &
nvwa_topology_pid=$!
# 验证后同样 kill -TERM "$nvwa_topology_pid"; wait "$nvwa_topology_pid"
```

启动器实际配置 worker：`-agent=room-audio -room-audio-bus=udp://127.0.0.1:PORT -frame-cache-off`。DC 另加 `-datachannel -datachannel-upstream=ws://127.0.0.1:ROOM_PORT/relais-upstream`；上限保持 1024 B / 16384 B / 4 channels。所有监听使用动态、互不重复的 loopback 端口；排除 6379、9101、9201。没有使用已有 Redis、Derek 的 Chrome 或共享 DevTools。

单次运行和故障复现：

```sh
"$nvwa_scratch/bin/driver" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/one-dc-normal" -datachannel \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg" \
 -count 1000 -rate 30 -fault normal
# -fault 可替换为 disconnect、worker-kill、lost-receipt。
# 去掉 -datachannel 即同协议 WS 对照。每次使用新的 -out。

wefty exec --detach --json --max-runtime 18m -- \
 python3 experiments/nvwa-room/scripts/run-matrix.py --scratch "$nvwa_scratch" --transport paired
# wefty inspect RUN_ID --json
# wefty logs RUN_ID --follow
# 仅需中止本任务时：wefty cancel RUN_ID

"$nvwa_scratch/bin/backpressure" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/pressure-dc" -datachannel \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg"
"$nvwa_scratch/bin/policies" -bin "$nvwa_scratch/bin" \
 -out "$nvwa_scratch/policies-dc" \
 -tone-a "$nvwa_scratch/tone-440.ogg" -tone-b "$nvwa_scratch/tone-880.ogg"
```

`driver` 正常结束会回收其拓扑。`worker-kill` 只向自身 `Child.Process` 对象发送 SIGKILL，记录 PID/时间；房间和 Redis 保持运行。不要按进程名或端口 kill，不要关闭机器网络。保留 scratch 供审查；不删除共享状态。进程回收证据是启动器的逐个 `Process.Wait`，不是全机器 PID 扫描。

## 2. 构建、启动、功能、故障、音频与测量状态

| 检查 | 状态 | 证据与边界 |
| --- | --- | --- |
| 构建 / vet | VERIFIED | 合并基线重新构建；实验包、workerapp vet 和 JS 语法检查通过；二进制和源码 hash 已保留。 |
| 实际启动 | VERIFIED | 每次独立 Redis/relay/control/signal/两 worker/房间进程。signal 验证 TLS 和 bearer；浏览器只持有本地 lobby capability。 |
| 房间功能 | VERIFIED | Go 双客户端的命令应用、固定 wave v1/起始 tick、stop、快照、查询、join/leave；两传输共用 Room.Handle 和同一去重账本。浏览器每传输每人 90 操作另有实际收据。 |
| 连接恢复 | VERIFIED（有界） | 仅脚本主动重建传输；保持 room_epoch、玩家身份、已确认动作和最新状态，generation 1→2，最终房间版本一致。不声称自动故障检测或原通道存活。 |
| 丢回执 | VERIFIED | 房间先应用并记录，故障丢掉一次收据，先 query 再同 ID 重试；每次一个 dedup、一个内容冲突，副作用只一次。 |
| Move / Drain | VERIFIED | Move 明确拒绝且原调用继续；房间记录 Drain 的 op 3/reconnect_required，新 signal session 恢复原身份及结果。详见下文。 |
| 双向音频 | VERIFIED（Go）；PARTIAL（浏览器） | Go 实收数据匹配对方的互斥 Opus 参考音，排除自身回声。浏览器 WAV 读取失败，不能算指定音调验收通过。不是人声质量验收。 |
| 测量 | VERIFIED（本机样本） | 逐操作 RTT、状态年龄、收据序列、去重、队列、恢复和包到达间隔。完整原始数据及独立导出检查已保留。全栈 RSS/队列占用未测。 |
| 默认媒体完整回归 | PARTIAL / FAIL | 两个指定 make 门都实际运行，均保留红灯；定向绿灯不覆盖全量失败。 |

应用语义：token 绑定服务端玩家；generation 不进入 `(epoch, player, command_id)` 去重键。相同 type/input_seq/payload digest 返回原结果，变化内容返回冲突。`applied:true` 只表示房间已应用并记账，不表示网络 Write/ACK。高频状态只保留新序号，重连不补播输入。房间重新构造会产生新 epoch，旧 epoch 请求返回显式 `epoch_reset`；这条合同仅有单元测试。**真实房间进程 kill/restart 后新 epoch/reset 试验：NOT-RUN**。内存账本不提供崩溃持久恢复。

**非本地使用限制：** resume token 是原型级 capability。`/lobby` 没有登录认证；`/relais-upstream` 从 query string 取得 session ID，不独立认证其来源；token 不轮换；最新 `join` 无条件获胜，没有 compare-and-set。loopback、Origin 检查及随机有期限 token 不能替代面向公网的认证、会话绑定和并发 takeover 协议。

### 故障 cut 与解释

以下三条描述历史 stage-2 矩阵，不能当作本次新增在途试验。审查指出前两条在请求之间切断，没有待确认命令，因此没有覆盖断线后的 retry/dedup。新增 writer 屏障及 WS 结果另列于第7节；DC 在途试验等待 W1 合并后执行。

- `disconnect`：第 250 号操作前关闭 A 的任务自有应用连接和媒体 PC，等待 250 ms，再通过 signal 新建。没有切机器网络。
- `worker-kill`：同一操作边界 SIGKILL 自建 w1，随后重建 A 的调用和房间连接。DC 与音频共用 PC，因此音频也中断。WS 本来独立于媒体 worker；为使用同一恢复合同，对照脚本也主动重连 WS。**这不是 WS 必须受 worker 故障影响的证据。**
- `lost-receipt`：第 250 号命令已提交后只丢其应用收据；等待 150 ms，查询、重试，再验证内容冲突。该 RTT 包含超时和恢复查询。
- 所有恢复均由脚本知道 fault cut 后执行；未测客户端自主发现故障的时间。音频中断列是最大编码包到达间隔，未把它当主观声音质量或源端音频延迟。

### Move / Drain / 背压

Move 返回 HTTP **500**，携带 typed code `data_session_not_movable`。这属于明确拒绝，不是成功或不确定迁移；原调用音频计数由 20 增至 36，应用快照仍可读取。随后对 w1 的 Drain 确实结束同一旧 session，亦验证该调用未迁至 w2。

Drain 返回 HTTP **200 / kind=ended**。房间实际记录同一 session 的 `op 3 / reconnect_required`。客户端随后经 signal 获得不同 session ID，以原 token 加入并获得 generation **2**，查询和重试仍返回原命令结果；应用计数仍为1、dedup为1。恢复 **212.705 ms**，跨会话音频包间隔 **227.727 ms**；B 接收 A 的 peer 包计数从 36 增至 61。终止原因在房间上游事件中可见；未声称旧 PC 能可靠将该原因直接通知浏览器。

| 传输 | 应用命令数 | 房间队列峰值 B / 年龄 ms | typed slow_consumer | peer包 A/B | 音频最大间隔 A/B ms |
| --- | ---: | ---: | --- | ---: | ---: |
| ws | 80 | 15349 / 529.384 | True | 71/72 | 21.328/21.708 |
| dc | 80 | 15026 / 555.569 | True | 85/87 | 60.886/35.730 |

背压暂停的是一个真实应用发送者，不是全机网络；producer 80 条命令仍全部应用。该检查证明房间队列的界限和媒体共存，不能代替所有 SCTP 缓冲饱和场景。两个背压短测早于正式配对矩阵，并与回归阶段共享本机资源；不以它们的延迟差异归因于传输。

### 浏览器：实际运行与失败复现

本次 stage-2 的具体授权允许经 wefty 启动两个任务自有 Chromium。各使用独立 scratch `--user-data-dir`、`--use-fake-device-for-media-stream`、`--use-fake-ui-for-media-stream` 和不同 `--use-file-for-fake-audio-capture` 文件，没有连接或关闭用户 Chrome。

```sh
wefty exec --detach --json --max-runtime 2m -- \
 python3 experiments/nvwa-room/scripts/browser-smoke.py --scratch "$nvwa_scratch" --transport ws
# 在同一新 scratch 中使用 --transport dc，启动器会使用另一组独立目录。
```

WS：`run_da8207ac1be5f505e032a6a3750b46ac`，29.295 s。DC：`run_f90b5c06a30226e4978a1240d4b56ef2`，13.858 s。两种传输均有两个不同的服务端玩家、同一 epoch；每人完成 45 个已应用命令和 45 个 state 回复，未报应用错误。

但两次声调验收都失败：`peer_tone_samples=0`、`peer_over_self_db=null`。Chromium 的原始错误为：

```text
ERROR:simple_sources.cc(32) Failed to read .../tone-440.wav as input to the fake device.
Try disabling the sandbox with --no-sandbox.
```

880 Hz 文件同样失败。这些历史运行只确认音频服务读取失败，当时未定位具体原因。本次已把 fixture 放在 wefty 运行工作目录并校验 PCM，仍复现打开失败；详见第7节。未添加 `--no-sandbox`，也未修改机器或常用浏览器配置。**有效指定输入的浏览器双向音调证明仍为 NOT-RUN**，需编排器提供允许 Chromium 音频进程读取 fake capture 文件的独立执行环境。页面启动与消息功能的成功不能补成音频成功。早先 Codex 沙箱的进程握手失败作为 stage-1 历史失败保留，未抹掉。

## 3. 同条件配对结果、缓冲预算与维护成本

配对 wefty：`run_73fde15b9fdcf201fa568aba4fa22040`，**succeeded**；24/24 独立试验完成。ledger 存活 1103.971 s 包含等待回归结束，不能当纯测量时间。24 次试验原始 duration 合计 **829.261 s**。

本次实际调度命令另加 `--after-regression "$nvwa_scratch/harness/result.json"`，并用 `--max-runtime 25m` 包含等待时间。独立复跑可用第1节的18分钟版本；它不包含等待。

固定配置：每客户端 1000 条编号操作（500 个可靠命令、500 个可替换状态），30 Hz；Opus 20 ms / 32 kb/s；A=440 Hz、B=880 Hz。同一组二进制、payload、房间代码、音频实现和故障 cut。WS/DC 相邻运行，并轮换谁先运行。本任务的完整重回归先结束，再开始配对测量；未做机器级负载隔离，不能将本机数值外推公网。

下表 A/B 是服务端 p1/p2。RTT 为应用收据时间；状态年龄是收到快照时“最新状态”的最大年龄，包含断连时对方最后状态继续变老的区间。队列列只量房间普通输出（含在写消息）；另有预留的终止错误帧。恢复列是 A 最后权威观察到新 join 快照的间隔，cut 到恢复另存 JSON。B 不重连。版本是两端最终同值。

| 运行 | RTT均值 A/B ms | RTT最大 A/B ms | 状态年龄最大 A/B ms | 队列 B / 年龄 ms | 恢复 A ms | 音频间隔 A/B ms | 版本 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| normal-1-ws | 1.190/1.058 | 94.902/80.250 | 110.830/110.123 | 952/13.841 | 0.000 | 236.943/156.268 | 2002 |
| normal-1-dc | 1.779/1.784 | 17.388/18.135 | 81.017/84.207 | 970/5.726 | 0.000 | 40.425/40.839 | 2002 |
| normal-2-ws | 0.740/0.744 | 11.809/13.551 | 80.403/78.234 | 952/4.999 | 0.000 | 60.337/57.844 | 2002 |
| normal-2-dc | 1.614/1.613 | 27.204/23.823 | 82.123/89.312 | 970/3.449 | 0.000 | 62.960/56.374 | 2002 |
| normal-3-ws | 0.751/0.726 | 13.449/13.928 | 75.931/83.461 | 952/2.733 | 0.000 | 40.938/40.609 | 2002 |
| normal-3-dc | 1.852/1.861 | 36.591/49.502 | 97.744/121.553 | 970/1.316 | 0.000 | 63.484/64.085 | 2002 |
| disconnect-1-ws | 0.585/0.802 | 11.990/24.216 | 334.524/310.838 | 943/3.644 | 309.263 | 517.745/419.697 | 2003 |
| disconnect-1-dc | 0.810/0.863 | 18.021/17.259 | 533.378/500.877 | 965/2.795 | 499.841 | 499.072/398.989 | 2003 |
| disconnect-2-ws | 0.618/0.664 | 15.720/15.239 | 332.111/312.491 | 897/8.229 | 307.734 | 519.367/400.051 | 2003 |
| disconnect-2-dc | 1.666/1.847 | 18.042/23.404 | 533.590/519.963 | 912/4.761 | 518.140 | 511.780/420.029 | 2003 |
| disconnect-3-ws | 0.513/0.642 | 8.367/12.544 | 335.903/304.964 | 947/5.482 | 304.157 | 507.983/437.743 | 2003 |
| disconnect-3-dc | 1.482/1.618 | 24.304/14.577 | 534.341/508.911 | 958/8.417 | 500.795 | 496.639/399.205 | 2003 |
| worker-kill-1-ws | 0.360/0.364 | 9.485/7.717 | 70.817/87.356 | 947/1.695 | 44.367 | 237.904/240.676 | 2003 |
| worker-kill-1-dc | 0.605/0.645 | 10.073/9.528 | 267.356/247.582 | 965/2.034 | 246.744 | 245.504/159.065 | 2003 |
| worker-kill-2-ws | 0.226/0.288 | 6.071/20.522 | 70.279/69.682 | 897/1.787 | 42.006 | 244.833/240.498 | 2003 |
| worker-kill-2-dc | 2.318/2.310 | 186.558/305.140 | 333.577/383.470 | 965/20.860 | 311.356 | 291.432/221.617 | 2003 |
| worker-kill-3-ws | 0.258/0.252 | 23.216/20.726 | 71.750/68.496 | 894/1.506 | 43.852 | 248.948/239.867 | 2003 |
| worker-kill-3-dc | 1.751/1.779 | 43.605/43.012 | 300.743/282.422 | 965/8.802 | 277.240 | 262.618/180.861 | 2003 |
| lost-receipt-1-ws | 0.831/0.700 | 151.105/6.627 | 167.204/74.044 | 952/2.991 | 0.000 | 45.823/39.657 | 2002 |
| lost-receipt-1-dc | 1.608/1.407 | 152.497/20.279 | 168.117/79.709 | 970/3.246 | 0.000 | 70.830/59.032 | 2002 |
| lost-receipt-2-ws | 0.702/0.542 | 150.560/5.810 | 167.119/71.959 | 952/4.602 | 0.000 | 31.648/30.565 | 2002 |
| lost-receipt-2-dc | 1.622/1.459 | 151.596/31.567 | 167.054/76.395 | 970/2.203 | 0.000 | 56.694/51.406 | 2002 |
| lost-receipt-3-ws | 0.657/0.492 | 151.122/6.354 | 166.669/84.311 | 952/2.213 | 0.000 | 73.884/70.418 | 2002 |
| lost-receipt-3-dc | 1.543/1.397 | 153.608/11.794 | 170.631/75.877 | 970/1.037 | 0.000 | 39.958/35.442 | 2002 |


历史每行均观察到 **1000/1000 操作完成、1000 次总命令应用、0/0 未恢复应用消息、0/0 重复收据、0/0 乱序收据、0 额外命令应用**。**证明边界：** 每个客户端仅一个顺序请求在途，故成功请求返回中的重复/乱序为0基本由驱动构造决定；这不是主动重复/乱序网络的覆盖。历史 disconnect/worker-kill 又恰在请求间隙切断，去重路径没有执行，不能据此声称“传输丢失时命令只执行一次”。只有历史 lost-receipt 直接覆盖了 query/retry/dedup。第7节新增真正待回执的 WS cut，仍不把收据为0外推为网络可靠性结论。normal/lost-receipt 两端收敛于版本2002；disconnect/worker-kill 收敛于2003。两端最终 state_seq=1000。每个重连行 A 重连1次、B为0；每个 lost-receipt 行 dropped_receipts=1、dedup=1、conflict=1。

共48,000条编号操作，Go 实收 **80,213** 个 peer 参考包，self/unknown均为0；独立校验 **480** 个实收 Opus 样本，均属于对方且不属于自己。两份参考集合互不相交，因此自己的 echo 不能满足此证据。音频包丢失率、源端音频延迟和人声质量未测。

简表给出观察范围，不是可靠性比例或分位数：

| 条件 | 传输 | 客户端平均RTT范围 ms | 最高单条RTT ms | 最大状态年龄 ms | 队列峰值范围 B | 恢复A范围 ms | 音频包间隔范围 ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| normal | ws | 0.726–1.190 | 94.902 | 110.830 | 952–952 | 0.000–0.000 | 40.609–236.943 |
| normal | dc | 1.613–1.861 | 49.502 | 121.553 | 970–970 | 0.000–0.000 | 40.425–64.085 |
| disconnect | ws | 0.513–0.802 | 24.216 | 335.903 | 897–947 | 304.157–309.263 | 400.051–519.367 |
| disconnect | dc | 0.810–1.847 | 24.304 | 534.341 | 912–965 | 499.841–518.140 | 398.989–511.780 |
| worker-kill | ws | 0.226–0.364 | 23.216 | 87.356 | 894–947 | 42.006–44.367 | 237.904–248.948 |
| worker-kill | dc | 0.605–2.318 | 305.140 | 383.470 | 965–965 | 246.744–311.356 | 159.065–291.432 |
| lost-receipt | ws | 0.492–0.831 | 151.122 | 167.204 | 952–952 | 0.000–0.000 | 30.565–73.884 |
| lost-receipt | dc | 1.397–1.622 | 153.608 | 170.631 | 970–970 | 0.000–0.000 | 35.442–70.830 |

原始结果见 [stage2/summary.json](stage2/summary.json)、[stage2/manifest.json](stage2/manifest.json)、`stage2/raw/*.json.gz`。包括每条操作的 RTT/结果、输入序号、实际收据 journal、房间版本/代次、session ID、包到达时间和有限的实收 Opus 样本。导出器独立检查每代 request ID 的重复/乱序、session 唯一性、kill PID 归属、恢复时 wave v1/start_tick=0 与 state_seq=250，以及音频字节确实来自对方。

反压、策略、浏览器收据和完整测试日志在 `stage2/auxiliary/`，校验值及原始 scratch 路径在 [stage2/auxiliary-manifest.json](stage2/auxiliary-manifest.json)。stage-1 旧报告保留为 [REPORT.stage1.md](REPORT.stage1.md)；旧端口分配冲突未混入或替换本轮行。

### 缓冲预算与严格边界

以下为配置/代码预算，不是整个进程 RSS 上限；只有房间队列字节/年龄进行了逐次峰值记录。W1 内部队列峰值、TCP/SCTP 实际占用和全进程 RSS 为 NOT-MEASURED。

| 层 | 预算 / 实际保证 |
| --- | --- |
| 房间应用消息 | 1024 B；输入状态 payload <=128 B；每连接 pending <=16 KiB，含在写普通消息及终止错误帧预留；最多128个普通输出条目。 |
| 房间总状态 | 两名玩家、每人4096条命令结果、16连接、120条输入/秒/连接及身份；音频只留两个最新 <=1200 B Opus 包，100 ms 过期；最多64条上游结束事件。 |
| W1 每 channel 输出 | 应用队列16 KiB，加 SCTP BufferedAmount 16 KiB；最多4 channel，两个预算各最多64 KiB/session；最多另有4×1024 B 已取出待写数据。测试实际开 control/state 两个 channel。 |
| W1 读取 / SCTP | 4×64 KiB 固定 channel 读取 scratch；SCTP receive window 64 KiB/association、默认每 stream 16个重组条目；这**不是严格分片字节上限**。SCTP/DCEP 各有8 KiB scratch，库 accept 队列16。 |
| worker 上游 WS | 无额外应用发送队列；序列化1 s 写；帧默认<=1281 B；bufio读/写各4 KiB，TCP内核缓冲另计。 |
| 已有 DTLS / session | raw record packetio 1,000,000 B；一个解密记录槽及<=8192 B输入记录；预握手加密队列<=100条/819,200 B；握手片段<2,000,000 B/1000片；session scratch为32,768 B，预密钥SRTP队列<=64 datagram/524,288 B。本轮frame-cache-off；对象、控制数据、caller及音频bus开销另计。 |
| 内核 UDP 配置 | 本地启动日志：relay public/leg 与两 worker 这四个 socket 各有效 read=8 MiB、write=4 MiB，共48 MiB配置容量；不表示占用或RSS。caller/音频bus及其它缓冲另计。 |

**承接 W1 的 HOLD，未修复：** 违反协商上限的 peer 发送分片 128 KiB 消息时，可能在 Pion 只交付完整消息前卡住 channel，不能及时得到 typed close；64 KiB receive window 不能补成硬 admission。该诊断由 W1 提供，本轮没有重跑或改 Pion。浏览器遵守 `max-message-size`，本轮合法小消息路径不依赖绕过此限制，但不能面向不可信公网 peer 宣称严格 1 KiB 入站保障。

W1 另保留会话生命周期策略：最多4次 channel admission，标签不复用；上游 WS 超大帧若无法安全取得 label，会关闭该 session 的所有 DataChannel，媒体继续。本轮每次只开 control/state 两个 channel，重连创建新 session，没有证明同 session 内标签重开。

### 复用、新增与成本判断

复用了 Relais 的 TLS/bearer signal、控制面放置、实际 relay/worker 媒体路径、Redis 所有权/代次，以及 legacy agent 接口。DataChannel 复用了同一 WebRTC PC，W1 新增 SCTP/DCEP、队列、上游 WS 适配和 data-session 禁迁移/终止策略。

权威房间、lobby/resume token、连接代次、命令账本/冲突、动作/状态协议、两种客户端和 recorder 都是新增应用工作。跨会话音频 UDP bus 也是本实验新增；Relais 原来的一 caller echo 没有自动变成多人房间或 SFU。它只替换最新 peer Opus 包，不做混音、重定时或质量保障。

直接 WS 的数据路径和故障域更简单，音频连接失败不必让应用连接重建。DC 增加 SCTP/DTLS 共享生命周期、额外 worker→room hop、缓冲预算和拒绝迁移策略；维护面更大。是否值得取决于复用 WebRTC 连接/客户端能力的具体收益。当前小样本不支持宣称部分可靠状态通道在丢包网络中更优；本轮没有模拟这种网络条件。

## 4. GO / HOLD / NO-GO

- **GO（窄范围）**：保留独立房间合同与 WS 基线；在本机、受信客户端、合法小消息、允许显式新连接的条件下继续消费端实验。Go 合成音频已证明 A→B/B→A，不是各自 echo。
- **HOLD（采用/发布）**：正式 Nvwa 或公网部署；需完成浏览器指定音频输入、严格分片 admission、独立审查，并查清完整媒体回归红灯。不能用定向复跑或单机矩阵代替这些门。
- **NO-GO（当前版本不承诺的能力）**：原 DataChannel/DTLS/SCTP 的透明迁移或崩溃续接；不可信 peer 的严格 1 KiB 分片保障；把内存账本当房间崩溃后的持久结果。当前实现采用显式拒绝/终止后重连，不能包装成这些能力已经完成。

## 5. 未验证清单

NOT-RUN：原生 Rust consumer、真实 Nvwa native/Web runtime、角色资产/骨骼播放；公网与 TURN/TCP/TLS fallback；跨主机、跨区域及全球网络；Safari/mobile；房间进程死亡后的持久恢复；负载容量/长期稳定性；原通道迁移。全栈内存/缓冲占用与真人音质也未验收。

浏览器真实消息路径已有90条/人/传输证据，但不等于完整浏览器故障矩阵或音频验收。Go/浏览器成功均不补成 Rust/Nvwa 完成。

## 6. 测试、真实失败与 CI

| 检查 | 实际结果 | 墙钟时间 / 条件 |
| --- | --- | --- |
| 重新构建、vet、JS语法 | PASS | 本机缓存依赖；未下载模块；实验源码/二进制 hash 保留。 |
| 705b3e2 实验包测试的旧声明 | **撤回 PASS** | 审查和本次定向复现均显示 `TestUpstreamSessionEndReasonIsRecorded` 失败；`Metrics().Events` 别名污染状态。旧整包绿灯声明不成立。修复后的实际验证另见第7节。 |
| make test-unit + 自建Redis | FAIL / PARTIAL | **251.257 s**；Redis 127.0.0.1:55351，自己启动并回收；GOMAXPROCS=4、go test -p 2。 |
| make test-harness-short | FAIL / PARTIAL | **653.657 s**；包测试644.857 s；内存模式，按Make目标清空Redis。 |
| TestStallIntervalsRecover 定向race/count=3 | PASS | **9.511 s**；不覆盖前一完整门的失败。 |
| relay-restart 定向race/short/count=3 | PASS | **8.666 s**；不覆盖前一完整门的失败。 |
| 配对矩阵、策略和背压 | PASS | 上述原始数值与可解压收据；每次任务进程均有自己的清理。 |

完整单元门的真实失败：`internal/udpbuffer` 的 `TestApplyReportsEffectiveSize`、`TestApplyReportsClampWithoutFailing` 在读取 `sysctl -n kern.ipc.maxsockbuf` 时 exit status 1，与 W1 已报告的沙箱限制位置一致；没有用 wefty 绕过这个已知限制。`internal/loadgen/TestStallIntervalsRecover` 的恢复窗口发送迟到25.974 ms（门槛20 ms）且出现跳帧，故红灯。

完整 harness 的真实失败：`TestRelayBufferFailurePaths/memory/relay-restart`，断言期望 `RelayReplayComplete=false`，实际为true。后来单独运行这个子例连续三次通过；发送节拍子例也三次通过。本次编排器转交的独立审查结论：`TestStallIntervalsRecover` 在 CI 通过；`relay-restart` 的 checkpoint 可在 kill 前落地，导致测试时序竞争。CI 的 unit-tests 红灯来自本分支未改动的 `pkg/relay/TestSelfFenceStopsBothPacketPathsAndCounters`。这些是编排器提供的 CI/审查证据，本 worker 未访问 CI 或修改相关生产/测试代码。第7节提供固定主线提交的本机5次对照；未声称已修复这些测试。

复现上述命名门：

```sh
python3 experiments/nvwa-room/scripts/regression-stage2.py --out "$nvwa_scratch/units" --gate test-unit
wefty exec --detach --json --max-runtime 20m -- \
 python3 experiments/nvwa-room/scripts/regression-stage2.py --out "$nvwa_scratch/harness" --gate test-harness-short
```

单元门直接运行是因为已知sysctl沙箱限制，未将它迁到wefty。长harness已用wefty；实际门槛以 result.json/FAIL日志为准。


默认行为的相关用例（echo旧快照/导出、V7 wire、audio-only、browser-like audio/video、Chrome offer等）均有通过记录，但不能据此把整个默认媒体回归填为 VERIFIED。完整日志为准。

本轮未修改失败测试、阈值或生产策略。也未修复 W1 的分片问题。新增的是房间对 frozen op2/op3 结束原因的有限记录及真实 WS 单测、DC 背压选择、Move/Drain 消费者、配对 runner、双传输浏览器选择与导出。`regression-stage2.py` 首版没有把 make 的非零状态传到 wrapper 进程，现已修复；本次旧 wrapper 的 wefty `succeeded` **只表示 wrapper 正常退出**，实际 harness `result.json.exit_code=2` 和 FAIL 日志决定门槛结论。

全部测试为本机直接执行或本机 wefty；没有触发 hosted CI、没有云资源或 paid API，没有新模块/工具链下载。独立代码审查仍由编排器安排，不在本报告中冒充完成。


## 7. 705b3e2 审查修复（W2，本次未提交）

### 代码与证据修正

- **VERIFIED：lint/打包。** HTTP 数字状态改为标准库常量；动作分支改为 switch；去掉未用字段。音频 agent 移至 `internal/spike/roomaudio`，worker 仅导入 internal 包；默认 echo 选择及 room-audio 开关保持原合同。`scripts/check-stock-worker.sh` 实际构建 stock worker，检查完整生产依赖及二进制符号均无 `experiments/`。internal spike 的 opt-in 实现仍链接在 stock binary 中，这是本次选择 internal 方案的明确边界，没有宣称使用 build tag 完全移除该实现。
- **VERIFIED：Metrics。** 修复前 `TestUpstreamSessionEndReasonIsRecorded -race -count=3` 三次均因内部切片被改成 `changed` 失败；现于锁内 `slices.Clone`，并加入真实 `/metrics` HTTP 编码与事件滚动并发测试。room/client 包 race 10次通过。已撤回第6节旧 PASS 声明。
- **VERIFIED（WS）；NOT-RUN（DC）：在途命令恢复。** `RequestWithSent` 在实际 Send 返回后发信号；测试命令经真实 room writer 暂停回执（`-test-faults` 下，最长2秒），`/metrics` 只读屏障证实命令已经应用，客户端原请求仍等待。随后断开或杀自有 worker；新 signal 会话/同 token/new generation 加入，先 query 后 retry 原命令；原结果必须一致、dedup=1、目标命令应用次数=1、全房间总应用=1000。没有用掉回执故障来代替此 writer 屏障。代码对 WS/DC 共用；本轮按派工只跑 WS 的3+3，等 W1 合并再跑 DC。仍每人1000操作、30 Hz，应用 payload 与两种音调不变；仅故障目标命令额外携带测试暂停字段。此 cut 固定在提交后/回执前，不覆盖提交前失败或自主故障检测。
- **NOT-RUN：真实 room 进程重启。** 第2节已明确 epoch/reset 只有单元覆盖；并列出无认证 lobby、query session、token 不轮换和 newest-join 无 CAS 的非本地风险。

本轮复现命令：

```sh
bash experiments/nvwa-room/scripts/build.sh "$nvwa_scratch"
python3 experiments/nvwa-room/scripts/run-matrix.py --scratch "$nvwa_scratch" \
 --transport ws --faults disconnect worker-kill
python3 experiments/nvwa-room/scripts/export-inflight.py --scratch "$nvwa_scratch" \
 --out experiments/nvwa-room/evidence/review-fixes
bash experiments/nvwa-room/scripts/check-stock-worker.sh "$nvwa_scratch/stock"
# 固定历史树，不写共享 worktree Git 元数据。
mkdir -p "$nvwa_scratch/main-7dfde0b"
git archive 7dfde0b | tar -x -C "$nvwa_scratch/main-7dfde0b"
python3 experiments/nvwa-room/scripts/compare-regressions.py \
 --main "$nvwa_scratch/main-7dfde0b" --out "$nvwa_scratch/main-comparison"
```

### WS 新增在途试验原始结果

每轮每人1000操作/30 Hz，总命令应用1000，A重连1次、B为0；原请求均 `connection_closed`。下表 query/retry 原结果版本相同，generation 均1→2，dedup均1、目标命令副作用均1，dropped_receipts均0。writer 屏障在切断前已被观察到，原命令回执在切断前的原始 journal 中不存在。导出器另核对 kill PID 属于本轮 worker-1，Redis 均非6379。

| 运行 | 命令 | Send→cut ms | 原结果版本 | dedup / 副作用 |
| --- | --- | ---: | ---: | ---: |
| disconnect-1 | p1-0250 | 0.831 | 503 | 1/1 |
| disconnect-2 | p1-0250 | 0.485 | 504 | 1/1 |
| disconnect-3 | p1-0250 | 1.438 | 503 | 1/1 |
| worker-kill-1 | p1-0250 | 1.866 | 504 | 1/1 |
| worker-kill-2 | p1-0250 | 5.042 | 504 | 1/1 |
| worker-kill-3 | p1-0250 | 0.814 | 504 | 1/1 |

A/B仍对应服务端p1/p2。恢复前者是最后权威观察→新join，后者是cut→新join；RTT包含该目标命令的重建/query/retry耗时。

| 运行 | RTT均值 A/B ms | RTT最大 A/B ms | 状态年龄最大 A/B ms | 队列 B / 年龄 ms | 恢复 / cut恢复 A ms | 音频间隔 A/B ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| disconnect-1 | 0.699/0.359 | 263.301/6.224 | 299.647/295.507 | 952/2.086 | 295.372/261.743 | 496.226/420.275 |
| disconnect-2 | 0.613/0.402 | 262.258/14.825 | 300.340/295.710 | 947/4.125 | 261.793/261.482 | 500.240/419.612 |
| disconnect-3 | 0.590/0.315 | 260.274/10.718 | 300.899/293.628 | 943/2.047 | 293.414/258.304 | 495.318/420.921 |
| worker-kill-1 | 0.751/0.807 | 13.725/18.335 | 82.884/71.431 | 947/4.522 | 12.030/10.468 | 234.006/221.402 |
| worker-kill-2 | 0.355/0.356 | 33.297/11.759 | 77.535/76.891 | 952/5.310 | 32.870/27.755 | 248.422/239.390 |
| worker-kill-3 | 0.414/0.331 | 10.327/4.226 | 71.563/72.758 | 894/3.037 | 9.549/8.977 | 241.019/259.942 |

六轮均 `missing=0/0`、成功请求返回中的 duplicate/reorder=0/0；后两者仍受串行 driver 构造限制，不是主动重复或乱序网络试验。此次真正执行了断线后同 ID 重试，所以“目标副作用一次”由该命令应用计数、全房间总计数和原结果一致性共同验证。没有推导可靠性比例或分位数。

原始全量收据/包到达与命令屏障时间：[review-fixes/raw/](review-fixes/raw/)；逐轮数值：[review-fixes/summary.json](review-fixes/summary.json)；gzip校验：[review-fixes/manifest.json](review-fixes/manifest.json)。此六轮是审查修复后的新记录，不替换 stage2 历史24轮。

### 浏览器 fixture 定位

WS `run_e0755648f6dfa76280dfde6e2570ef87`、DC `run_a2c8160a77c77b2eb1dd3f68bcdbfd88` 均实际重试并退出1。每个新 WAV 在 **wefty 执行工作目录** 生成，存在且 launcher 可读；canonical RIFF PCM signed little-endian、16 bit、单声道、48,000 Hz、96,000 frames、192,044 B，校验及 SHA-256 保留在 `review-fixes/browser-{ws,dc}/tone-*-fixture.json`。无需从调用方 scratch 带入。每传输两个玩家仍各完成90条应用操作，但 peer_tone_samples 均为0，不能通过音调验收。

新路径仍报：

```text
ERROR:simple_sources.cc(32) Failed to read .../work/nvwa-browser-fixtures-.../tone-440.wav as input to the fake device. Try disabling the sandbox with --no-sandbox.
```

880 Hz 相同。Chromium 的 [ReadWavFile](https://raw.githubusercontent.com/chromium/chromium/main/media/audio/simple_sources.cc) 在文件打开失败时打印此错误，WAV 解析失败是另一条错误。上游 [macOS fake capture 沙箱说明](https://chromium.googlesource.com/chromium/src.git/+/167f34ae932ed8f702d9127339acbff246b4eb47) 记录同一限制。已排除本轮 fixture 缺失和 PCM 格式错误；观察与 Chromium 音频进程自己的 macOS 沙箱拒读一致，未采集系统审计日志或 errno，故不把推断包装成 OS 审计证据。

Chromium 的 [WAV validator](https://raw.githubusercontent.com/chromium/chromium/main/media/audio/wav_audio_handler.cc) 接受16-bit PCM 与非零采样率，FileSource 会作音频转换；并没有固定必须16 kHz的合同。本次使用48 kHz并明确验证，不靠 ffmpeg 的默认输出。未添加 `--no-sandbox` 或修改任何用户浏览器配置。**浏览器指定音调：PARTIAL（启动/消息已验证；音调实际尝试失败），成功的双向音调证明仍 NOT-RUN**。仅在 Codex 外重跑未必解决 Chromium 自己的沙箱限制；需要编排器选择允许 fixture 文件的测试执行方式。


### 主线与本分支的两项红灯对照

固定 `7dfde0b7fbb52debf0651b5f23766e1ab09dd9fd` 的 `git archive` 内容置于 scratch，未切换当前 worktree。先完成编译，然后 main/branch 交替顺序执行；均 `GOMAXPROCS=4`、`-race -short`，harness 固定 memory、无 Redis，本轮不同时运行其它自有测试门或矩阵。前一轮伴随构建的预热试验未作下表数据。两个目标的测试源文件与该主线相同。

| 测试 | 树 | 5次退出码 | 5次墙钟秒 |
| --- | --- | --- | --- |
| stall | main | 0,0,0,0,0 | 4.053, 3.765, 3.745, 3.758, 3.752 |
| stall | branch | 0,0,0,0,0 | 4.058, 3.744, 3.811, 3.747, 3.750 |
| relay-restart | main | 0,0,0,0,0 | 3.853, 3.358, 3.377, 3.334, 3.336 |
| relay-restart | branch | 0,0,0,0,0 | 3.712, 3.334, 3.449, 3.346, 3.339 |

**VERIFIED：这20次定向执行均通过；PARTIAL：旧全套负载下的失败归因。** 本轮 main 各5次均未失败，branch 也各5次未失败；所以没有通过本机定向试验复现“主线也失败”。未重建历史完整 make 的包并发/瞬时机器负载，不把这组绿灯当作“历史时序竞争已消失”的证明。编排器提供的 CI/独立审查结论另列第6节；未触发新的 hosted CI。

原始命令、每次输出和时间：[review-fixes/main-comparison/summary.json](review-fixes/main-comparison/summary.json) 及同目录日志。

### 本次验证门

| 门 | 状态 | 墙钟秒 |
| --- | --- | ---: |
| vet | VERIFIED / PASS | 0.889 |
| lint | VERIFIED / PASS | 1.270 |
| race-10 | VERIFIED / PASS | 3.010 |
| stock | VERIFIED / PASS | 0.349 |
| diff-check | VERIFIED / PASS | 0.021 |

`go vet -mod=readonly ./...`；golangci-lint **v2.14.0** 对 `./experiments/... ./pkg/workerapp/... ./internal/spike/roomaudio/...`，新建专用缓存，未排除实验目录；room/client 均 race/count=10，包含新的真实 HTTP 并发编码与在途 WS 测试。stock build 检查的是生产依赖和最终符号。原始输出与命令见 [review-fixes/verification.json](review-fixes/verification.json)。移包后的 audio-agent 及 workerapp race/short 包测试也实际通过；本次没有重跑两个完整 make 门或完整 DC 矩阵。

新浏览器 ledger 墙钟 WS **17.991 s**、DC **12.021 s**；它们都是 failed/exit1，不能作为音调 PASS。wefty 使用明细在派工 scratch `wefty-exec-log.md`。


独立审查另发现的四项生产问题（16条重组上限遇突发17条、flag-OFF Drain、第五条 channel 结束媒体、Move 应返回409）由 W1 在其它 worktree 负责。本次未修改相关生产文件，也未在本分支声称这些问题已经修好；合并后的验证是后续门。第3节旧 buffer 表及 HTTP500 仅代表历史测量基线，不能冒充合并后结果。
