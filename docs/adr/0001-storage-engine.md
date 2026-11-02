# ADR 0001: 存储引擎选用 Pebble v2

- 状态：**已接受**（2026-11-02，人类明确选择）
- 日期：2026-11-02
- 层级：A（新增第三方依赖）
- 相关不变量：INV-S1、INV-S2
- 相关文件：`go.mod`、`internal/storage/`、`docs/design/storage.md`

## 背景

`storage` 是依赖图最底层，必须为 `mvcc`（进而为 `raftstore` / `txn`）提供：

1. 按字节序的有序 KV 读写；
2. 一致性快照与前向迭代；
3. 原子批写——一次 apply 要同时写入 data / lock / write 多条记录；
4. 持久化——进程重启后从同一路径恢复。

`mvcc` 的 key 编码依赖“字节字典序 = 迭代顺序”（INV-S2），因此引擎的 comparer 必须等价于 `bytes.Compare`。

本项目的目标是自研 Raft、MVCC 与 Percolator 两阶段提交，**不是自研存储引擎**。

## 决策

采用 **`github.com/cockroachdb/pebble/v2`，版本锁定 `v2.1.7`**，并封装在 `internal/storage` 接口之后（接口契约见 `docs/design/storage.md`）。

## 为什么是 Pebble

| 驱动因素 | 说明 |
|---|---|
| 字节序天然匹配 | Pebble 默认 comparer 即 `bytes.Compare`，直接满足 INV-S2，无需自定义编码适配 |
| 能力齐全 | 有序迭代、一致性快照、原子 batch、WAL、崩溃恢复、compaction 均已成熟；M1 只需定义契约 |
| 纯 Go、无 cgo | 构建、测试、部署简单；测试直接用 `t.TempDir()`，不引入外部服务 |
| 成熟度 | CockroachDB 生产使用，维护活跃；v2 是当前主线 |
| 边界清晰 | Pebble 不包含 raft / 事务协议，不会把本项目要自研的共识逻辑带进来 |

### 为什么锁定 v2.1.7

决策时（2026-11-02）v2 线最新版本为 `v2.1.7`，v1 已停止跟进。`go.mod` 精确锁定该版本；升级前必须重新验证本 ADR 依赖的语义假设（comparer、snapshot、iterator、batch）。

### 与 AGENTS.md §8.7 的关系

§8.7 禁止的是用 etcd/raft、tikv 等现成实现**替代自研的共识/事务模块**。Pebble 是底层存储引擎，不提供 Raft 或事务协议，且本依赖已由人类在 2026-11-02 明确授权（AGENTS.md §5.9 要求的新增依赖确认）。

## 备选方案

| 方案 | 优点 | 否决理由 |
|---|---|---|
| 纯内存有序引擎 | 最简单、启动最快 | 不持久化；重启恢复（INV-R4 的 `last_applied`）需要持久状态，后续仍需更换引擎 |
| 自研 WAL + SSTable | 完全自研、完全可控 | 工作量与正确性风险大（原子性、恢复、compaction），会显著推迟 MVCC / Raft 主线；不是项目目标 |
| 直接把 Pebble API 暴露给上层 | 少一层封装 | 第三方类型跨层泄漏，语义随 Pebble 版本漂移；违反 AGENTS.md §2 的接口约束 |

## 后果

**正面**

- M1 代码量集中在契约与语义上，持久化与恢复直接可用。
- 测试无需外部依赖，`go test -race ./...` 即可覆盖。

**负面**

- 引入约 30 个传递依赖（见 `go.sum`），存在供应链与升级成本。
- `storage` 的部分行为由 Pebble 定义；必须用我们自己的黑盒测试锁定契约语义。
- “自研”边界从此明确：**共识、MVCC、事务自研；存储引擎是第三方依赖**。

## 重新评估的触发条件

1. 需要多进程同时打开同一路径（当前单进程假设）。
2. 需要 Pebble 不提供的操作（范围删除、插件化存储等）。
3. Pebble 升级改变了 comparer / 快照 / 迭代 / batch 语义。
4. 项目目标变为“存储层也必须自研”。

## 验证

```bash
go get github.com/cockroachdb/pebble/v2@v2.1.7   # 依赖可获取
go test -race ./internal/storage                 # 契约由 docs/design/storage.md 列出的测试覆盖
```