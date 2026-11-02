# AGENTS.md — raftkv

> 本文件是 AI 编码代理的行为约束,所有 agent 必须遵守。
> 核心原则:**人类拥有设计与接口,AI 只拥有实现。**
> 跨层接口的定义、签名与语义变更一律按 A 层处理(见 §2、§3、§9)。
> 违反本文件任一条,产出视为无效,必须回退重做。

---

## 1. 项目是什么

**raftkv** —— 一个基于 Raft 的分布式事务 KV 存储(mini TiKV),Go 实现。

- 数据按 key range 划分为多个 **region**,每个 region 是独立的 Raft group
- 状态机使用 **MVCC**:`data` / `lock` / `write` 三类记录
- 跨 region 事务使用 **Percolator 两阶段提交**,隔离级别为**快照隔离**
- 时间戳由 **TSO** 统一分配,严格单调

**它不是什么**:不是 SQL 数据库、不保证可串行化、不是 etcd/TiKV 的 fork、第一版不做 region 迁移。

---

## 2. 架构与依赖方向

```
server (gRPC / 路由)
  ├── txn (Percolator / latch / ResolveLock)
  │     ├── mvcc (版本编码 / data+lock+write)
  │     │     └── storage (有序 KV / 迭代器 / 快照)
  │     └── raftstore (region / Propose / ReadIndex)
  │           └── raft
  ├── tso
  └── pd
client (事务 API / 2PC 驱动 / 重试 / 路由缓存)
```

**依赖规则(强制):**

1. 只允许上层依赖下层,禁止反向 import。
2. 跨层只能通过接口:`Storage`、`StateMachine`、`TimestampOracle`、`RegionRouter`。
3. `txn` 不得 import `raft` 包,只能通过 `raftstore` 的接口。
4. `mvcc` 不得 import `txn`:状态机不知道"事务"这个概念,只认识 lock/write 记录。
5. 跨层接口的定义、签名与语义变更一律按 A 层处理,不论接口所在文件属于哪一层。

---

## 3. 任务分层:哪些文件 AI 不能碰

### A 层 —— 人类拥有,未经明确授权禁止修改

| 文件/路径 | 原因 |
|---|---|
| `internal/raft/**` | 共识逻辑;AI 不得重写 |
| `internal/mvcc/encoding.go` | key/ts 编码决定迭代语义与正确性 |
| `internal/mvcc/state_machine.go` | apply 分发,确定性要求最高 |
| `internal/mvcc/version.go` | 版本读取与可见性判断 |
| `internal/txn/percolator.go` | 两阶段提交协议状态机 |
| `internal/txn/resolve.go` | 锁解析,崩溃恢复的正确性核心 |
| `internal/txn/latch.go` | 加锁顺序决定是否死锁 |
| `internal/tso/oracle.go` | 时间戳单调性 |
| `internal/raftstore/read_index.go` | 线性一致读 |
| `internal/types/**` | `Region` / `RegionEpoch` / `TxnStatus` 等共享语义类型 |
| `proto/**/*.proto` | 线协议与接口契约 |
| `DESIGN.md`、`docs/adr/**`、`AGENTS.md` | 设计与规则文档 |

### B 层 —— AI 实现,人类逐行审

`internal/storage/**`、`internal/raftstore/**`(除 `read_index.go`)、`internal/server/**`、`internal/client/**`、`internal/pd/**`、`internal/tso/**`(除 `oracle.go`)、`internal/metrics/**`、`internal/txn/**` 中未列入 A 层的文件。

> `internal/raftstore/apply.go` 虽在 B 层,但凡影响 apply 顺序、幂等或 ReadIndex 等待的改动,按不变量触发条款走 A 层。`internal/txn` 中影响 latch 获取顺序的调用方同理。
> `internal/metrics/**` 中,apply 路径内的埋点只允许原子计数,不得阻塞、不得影响状态。

### C 层 —— AI 实现,人类扫一眼

`deploy/**`、`scripts/**`、`Makefile`、CLI、生成的 `*.pb.go`、metrics 面板、仅用于非正确性测试的数据生成器。

> `Makefile` 中 test / lint / CI target 的修改需授权。

### 默认规则与不变量触发条款

- **默认规则**:未列出的文件默认 B 层;新增包默认 A 层。
- **不变量触发条款(优先于文件清单)**:任何改动只要可能影响以下任一项,无论涉及哪个文件、位于哪一层,一律视为 A 层,必须先获得人类授权:
  1. Raft 安全性(选举、日志匹配、提交索引、快照边界与原子安装)— INV-R*
  2. 状态机确定性与幂等性 — INV-R4
  3. MVCC 可见性与版本单调 — INV-M*
  4. Percolator 提交点与锁解析 — INV-P*
  5. TSO 单调性与不回退 — INV-T*
  6. latch 的获取顺序与持有范围 — INV-L*
  7. 读路径:ReadIndex 与 applied index 等待 — INV-RD*
  8. storage 编码、迭代一致性与快照 — INV-S*
  9. GC / safe point(引入后)— INV-GC*
  10. 跨层接口的定义、签名与语义
  11. `proto` 字段与共享类型
  12. 配置默认值(TTL、锁超时、批量大小)
- 文件归属是**第一判据**,不变量是**最终判据**;两者冲突时以触发条款为准。

---

## 4. 核心不变量(基线)

> 本清单是基线;新增、删除或修改任一条,需人类确认。其他规则应引用 INV 编号,而不是重复描述。
> 约定:`read_ts` 指本次读使用的时间戳——事务读时为该事务的 `start_ts`,外部读时为本次新分配的 ts;`lock.start_ts` 指持锁事务的 `start_ts`。

### Raft

- **INV-R1 Election Safety**:同一 term 至多一个 leader。
- **INV-R2 Log Matching**:两个日志在相同 index + term 上的 entry 相同,则该 index 之前的所有 entry 相同。
- **INV-R3 Leader Completeness**:已提交的 entry 出现在所有更高 term 的 leader 日志中。
- **INV-R4 State Machine Safety**:同一 index 的 entry 在所有节点 apply 的内容相同;apply 顺序与日志一致;重启后从持久化的 `last_applied` 继续,不重复生效。
- **INV-R5 Snapshot Atomicity**:快照安装是原子的;安装后的状态与 `last_applied` 一致;快照包含 data / lock / write 三类记录。

### MVCC

- **INV-M1 可见性**:读 `read_ts` 只能看到 `commit_ts <= read_ts` 的已提交版本。
- **INV-M2 未提交隔离**:未提交的 data / lock 只对写事务自己可见;读遇到锁时,只有 `lock.start_ts <= read_ts` 才需要解析,`lock.start_ts > read_ts` 的锁属于读者快照之外的未来事务,应忽略。
- **INV-M3 版本单调**:同一 key 的 `commit_ts` 严格递增;写冲突判定规则唯一且可复述。

### Percolator

- **INV-P1 唯一提交点**:primary lock 的 commit 记录是事务的唯一提交点。
- **INV-P2 前滚条件**:只有看到 primary 的 commit 记录,才能把事务 resolve 为已提交。
- **INV-P3 回滚条件**:TTL 过期且 primary 无 commit 记录,才能回滚。
- **INV-P4 无部分可见**:任何时刻不允许外部读到事务的部分写入。

### TSO

- **INV-T1 严格递增**:分配的时间戳严格递增。
- **INV-T2 重启不回退**:重启后不得回退;必须持久化已分配 horizon。
- **INV-T3 提交时间戳**:`commit_ts > start_ts`;物理时钟回拨时取 `max(last, now)`。

### latch

- **INV-L1 latch 不提供正确性**:latch 只做进程内并发控制;正确性由复制状态中的 lock 记录保证。
- **INV-L2 不跨调用持有**:不得跨 RPC / TSO 调用持有 latch。
- **INV-L3 顺序一致**:获取顺序全局一致(按 key 排序)。

### 读路径

- **INV-RD1 线性一致读**:外部读必须在 leader 上执行;先取 read index,等待 `applied index >= read index`,再按 `read_ts` 读取快照。禁止绕过此流程直接读本地状态。apply 内部的读不受本条约束。

### storage

- **INV-S1 迭代器快照一致**:迭代器看到的是快照一致视图。
- **INV-S2 编码顺序一致**:编码顺序与迭代语义一致(时间戳使用按位取反的大端编码)。

### GC(引入后生效)

- **INV-GC1 Safe Point**:safe point 不得越过任何活跃事务的 `start_ts`;只能删除 safe point 以下的旧版本;GC 必须幂等且可重试。

---

## 5. 铁律

1. **先设计,后编码。** 写代码前先给出:实现方案、涉及的不变量、三个最可能的失败场景。人类确认后才动手。
2. **apply 必须确定性。** 禁止 `time.Now()`、`rand`、map 遍历顺序、环境变量、网络 IO 进入 apply 路径。
3. **所有状态变更必须经过 Raft 日志。** 禁止在 apply 之外修改状态机;latch 可以是本地内存,lock 记录必须是复制状态。
4. **时间戳只能来自 TSO。** 禁止本地生成 `start_ts` / `commit_ts`。
5. **读必须线性一致。** 读状态机前必须经过 `ReadIndex`,禁止直接读本地状态。
6. **语义变更必须先提议。** 不得静默修改已有接口、数据结构或错误码。
7. **一次只做一个逻辑变更。** 单次 diff 原则上不超过 300 行,禁止大范围重写。
8. **修 bug 前先有根因假设。** 人类给出假设(或 AI 提出并等待确认)后才能改;禁止"试试这样"式修改。
9. **新增第三方依赖需先确认。** 禁止引入 etcd/raft、tikv 等现成实现来替代自研模块。
10. **可讲清才合入。** 每个模块人类必须能不看代码讲 2 分钟;讲不清就不合。

---

## 6. 标准工作流(每个模块)

```
① 人类写接口 + 数据结构定义
② 人类写:不变量、错误分支、3 个测试场景
③ AI 只实现该接口,不改其他文件
④ 人类先写测试,再读 AI 代码
⑤ 逐行 review,有疑问的行加注释写明"为什么"
⑥ 人类关掉代码,1 页纸默写该模块流程
⑦ 对照代码,差异即知识漏洞
```

AI 在第 ③ 步之前,必须先复述:目标、涉及模块、不变量、预计改动的文件清单。

---

## 7. Definition of Done

任何改动合入前必须满足:

- [ ] 写明了它保护/维持的 **INV 编号**
- [ ] 测试覆盖:正常路径、边界、故障场景(场景由人类设计)
- [ ] 确认 apply 路径没有非确定性输入
- [ ] 人类能不看代码讲清这段逻辑
- [ ] 涉及设计决策时,已新增/更新 `docs/adr/`
- [ ] 单一逻辑变更、独立 commit,message 写明不变量;提交时机与权限按 §12
- [ ] 若涉及跨层接口,已获得人类授权

---

## 8. 禁止事项(逐条硬性)

1. 在 apply 路径使用时钟、随机数、map 遍历顺序、外部 IO。
2. 在 apply 之外修改状态机。
3. 绕过 `ReadIndex` 直接读本地状态。
4. 修改或删除已有测试断言来让测试通过;跳过 flaky 测试。
5. 把 lock 记录放在节点本地内存。
6. 本地生成 `start_ts` / `commit_ts`。
7. 引入 etcd/raft、tikv 的代码或依赖替代自研实现。
8. 一次性生成多个模块,或大范围重写文件。
9. 吞掉 error,或用 panic 处理可恢复错误。
10. 未经确认新增第三方依赖。

---

## 9. 交互协议

- **信息不足时必须提问**,不要猜。
- 涉及 A 层文件,或跨层接口的定义/签名/语义变更(**不论文件位于哪层**):**立即停止并请求授权**。
- 提交前输出:改动清单、保护的不变量、如何验证、未覆盖的风险。
- 非显然的设计选择:引用来源(Percolator 论文、Raft 论文、TiKV design doc)并写入 ADR。
- 不要生成"看起来对但未验证"的代码;给出验证方法,而不是断言它正确。

---

## 10. 术语约定

- **模块**:一个 Go package,或一个可独立描述不变量的逻辑单元。
- **语义变更**:`proto` 字段、错误码、公开接口、CLI flag、metric 名、日志字段的变化。
- **可恢复错误**:能带错误码返回并由调用方处理的错误;参数错误、not found、权限拒绝都属于此类,不得 panic。panic 仅用于不变量被破坏或程序错误。
- **latch vs lock**:latch 是进程内并发控制,不复制;lock 是事务锁记录,必须经 Raft 复制。
- **region / store / peer**:region 是 key range 的复制单元;store 是承载多个 region 的进程/节点;peer 是 region 在一个 store 上的副本。

---

## 11. 目录约定

```
raftkv/
├── AGENTS.md
├── DESIGN.md
├── docs/adr/
├── proto/
├── cmd/{pd,tso,store,client}/
├── internal/{raft,raftstore,storage,mvcc,txn,tso,pd,server,client,metrics,types}/
├── tests/{linearizability,chaos,bench}/
└── deploy/
```

---

## 12. Git 与代码管理

### 12.1 提交时机

- **逻辑变更完成且测试通过**后立即提交:一个逻辑变更对应一个 commit(与 §5.7 的 diff 上限一致)。
- **模块达到 §7 全部 DoD 条目**后,必须产生一个独立 commit,作为该模块的可回溯里程碑。
- 测试未通过、DoD 未满足、或改动尚未自证可工作的代码,**不得提交**。
- 多个无关逻辑变更不得混入同一个 commit;需要拆分时用 `git add -p`,禁止 `git add -A` 夹带。

### 12.2 提交权限

- **本地 commit**:AI 可在上述时机自主执行 `git add` / `git commit`,无需逐次询问。
- **推送与远端操作**:`git push`、创建/合并 PR、新增或修改 remote、force-push、改写已推送历史,**必须先获得人类明确确认**。
- 用户要求"叫我提交"时,AI 只输出 commit message 与待执行命令,由人类亲自执行。

### 12.3 Commit message 规范

```
<area>: <一句话变更>

保护的不变量:INV-XX / INV-YY
验证方式: <测试命令或场景>

Co-Authored-By: Claude Code <noreply@anthropic.com>
```

- `area` 取模块名:如 `mvcc`、`txn`、`raftstore`、`storage`、`tso`。
- 首行不超过 72 字符,只描述"做了什么",不回述 diff。
- 是否涉及 A 层文件或不变量触发条款,必须在正文写明。

### 12.4 硬性禁止

- 提交未通过测试的代码,或在提交前临时修改/删除测试断言使其通过。
- 提交密钥、token、本地配置、构建产物、依赖缓存等非源码文件。
- 使用 `--no-verify` 跳过检查,或 `--force` 覆盖已推送历史(除人类明确要求)。
- 在未获授权的情况下推送到 GitHub 或修改远端仓库。