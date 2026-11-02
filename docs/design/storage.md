# storage 设计说明

> 本文是**实现设计说明**，不是 ADR。ADR 只记录架构设计、组件选型等关键决策（见 `docs/adr/`）。
> 接口的权威定义以 `internal/storage/storage.go` 的注释为准；本文补充决策理由、失败场景与测试场景。

- 日期：2026-11-02
- 层级：A（跨层接口）
- 相关不变量：INV-S1、INV-S2
- 相关文件：`internal/storage/storage.go`、`internal/storage/pebble.go`（待实现）
- 引擎选型：见 [ADR 0001](../adr/0001-storage-engine.md)

## 背景

即使底层选了 Pebble（ADR 0001），`mvcc` 与 `raftstore` 也不应依赖 Pebble 的类型或行为。项目必须拥有一份自己的契约，使得：

- 上层只依赖 `storage` 包定义的类型；
- 未来替换引擎时，只改 `pebble.go`，不改上层；
- INV-S1 / INV-S2 由接口测试固定，而不是“Pebble 碰巧如此”。

## 契约要点

1. **接口封装**：`pebble.*` 类型不出包；构造函数固定为 `Open(Options) (Storage, error)`。
2. **读视图复用**：`Storage` 与 `Snapshot` 都实现 `Reader`（`Get` + `NewIterator`）。
3. **范围半开**：迭代范围 `[LowerBound, UpperBound)`；`len(x)==0` 表示该端无界；`lower >= upper` 返回立即无效的空迭代器，不报错。
4. **所有权**：`Get`、`Iterator.Key/Value` 返回副本，调用方拥有；`Write` 返回后不保留入参引用。
5. **只前向迭代**：第一版只有 `SeekGE` + `Next`；新增 `Prev` / `Last` 属于 A 层接口变更。
6. **原子批写**：`Write` 全部生效或全部不生效；空批是合法 no-op；未知 `Mutation.Kind` 返回 `ErrInvalidMutation`；`WriteOptions.Sync` 控制返回前是否落盘。
7. **快照可失败**：`NewSnapshot() (Snapshot, error)`，`Storage` 关闭后返回 `ErrClosed`。
8. **关闭语义**：`Close` 幂等；关闭后所有方法返回 `ErrClosed`，不得 panic；未关闭的快照与迭代器随 `Storage.Close` 失效。
9. **错误集**：`ErrNotFound`、`ErrClosed`、`ErrInvalidMutation`，均可用 `errors.Is` 判断。
10. **单进程假设**：同一路径不允许多进程同时 `Open`；需要时另行设计。

## 决策理由与备选

| 决策 | 备选 | 选择理由 |
|---|---|---|
| 返回副本 | 借用视图（零拷贝） | 消除一整类“Next 后旧切片失效”的脏读与竞态；分配开销留待后续按 A 层流程优化 |
| 半开区间 | 闭区间 | 与 Go/Pebble 惯用一致，便于表达前缀区间；上界 key 自然被排除 |
| `len==0` 无界 | 显式 HasLower/HasUpper 标志 | 接口更小；MVCC key 非空，不存在“以空 key 为界”的歧义 |
| 只前向 | 双向迭代 | 当前 MVCC 读与 Raft 快照都只需前向；减少接口面，反向遍历将来按需扩展 |
| `NewSnapshot` 返回 error | 无 error | 关闭后是可恢复错误，应返回 `ErrClosed` 而不是 panic 或返回 nil |
| `Sync` 由调用方决定 | 永远同步 / 永远异步 | Raft 日志与 apply 对持久性的要求不同，由调用方按场景决定 |
| 原子 `Write` | 单条写接口 | apply 必须把多条 MVCC 记录作为整体生效，这是 INV-P4 的基础 |

## 保护的不变量

- **INV-S1**：快照创建后的写入对快照不可见；迭代器看到快照一致视图。
- **INV-S2**：迭代顺序等于 key 的字节字典序；依赖 Pebble 默认 comparer。

## 三个最可能的失败场景

1. **上界边界实现成闭区间** → MVCC 扫描多读下一个 key，破坏 INV-S1/S2。
2. **快照/迭代器生命周期错误**（未 Close、关闭顺序错误）→ `DB.Close` 阻塞或 panic、资源泄漏。
3. **返回内部切片引用** → `Next()` 后旧 `Key/Value` 失效，调用方保存后读到脏数据或触发竞态。

## 测试场景

1. **正常**：批量 Put 后 Get；`[a,c)` 扫描返回有序的 `a,b`；快照建立后写 `d`，快照看不到、新读看得到；同批 Put+Delete 原子生效。
2. **边界**：空 `muts`；不存在的 key；空范围；`lower==upper`；以 `0xff` 结尾的 key 与上界；上界 key 恰好被排除；删除不存在的 key 成功。
3. **故障**：`Close` 后 `Get`/`Write`/`NewIterator`/`NewSnapshot` 返回 `ErrClosed`；快照关闭后读报错；迭代器关闭后 `Valid()==false`；`Sync=true` 后重新 `Open` 数据仍在；未知 `Mutation.Kind` 整批不生效。

验证命令：

```bash
go build ./...
go vet ./...
go test -race ./...
```

## 后果与未覆盖

- 副本带来额外分配；若将来需要零拷贝，属于 A 层契约变更。
- 仅前向迭代；GC / 反向扫描需要扩展接口。
- 单进程假设；多进程或多实例共享目录需要另行设计。
- 未提供范围删除；GC 里程碑（INV-GC*）再设计。

## 已确认的决策

2026-11-02 人类确认接口通过：

1. `NewSnapshot` 返回 `(Snapshot, error)`。
2. `Iterator.Key/Value` 返回副本。
3. 半开区间 + `len==0` 无界。