// Package storage 定义 raftkv 最底层的存储抽象：有序 KV、原子批写与一致性快照。
//
// 本文件属于 A 层（跨层接口）：接口的定义、签名与语义变更必须先获得人类授权。
// 具体引擎实现（当前为 Pebble）位于 pebble.go，Pebble 类型不得泄漏到本包之外。
// 构造函数约定为 Open(Options) (Storage, error)，由 pebble.go 提供。
package storage

import "errors"

var (
	// ErrNotFound 表示读取的 key 不存在。
	ErrNotFound = errors.New("storage: key not found")
	// ErrClosed 表示在 Storage/Snapshot/Iterator 关闭后继续使用。
	ErrClosed = errors.New("storage: closed")
	// ErrInvalidMutation 表示 Write 的输入包含非法 mutation；整批不生效。
	ErrInvalidMutation = errors.New("storage: invalid mutation")
)

// Options 是 Open 的配置。第一版只有 Path；新增配置项按 A 层流程追加。
type Options struct {
	// Path 是存储目录，必须非空。目录不存在时由实现负责创建。
	Path string
}

// Storage 是有序 KV 存储。
//
// 并发语义：Storage 与 Snapshot 的方法可并发调用；Iterator 不可并发使用。
// 生命周期：Close 幂等；关闭后所有方法返回 ErrClosed，不得 panic。
type Storage interface {
	Reader

	// Write 原子地应用一批 mutation：全部生效或全部不生效。
	// muts 为空时是合法 no-op。返回后实现不保留 muts 及其元素的引用。
	// mutation 非法（例如未知 Kind）时返回 ErrInvalidMutation。
	Write(muts []Mutation, opts WriteOptions) error

	// NewSnapshot 返回一个一致性读快照，调用方负责 Close。
	// 快照建立之后的写入对快照不可见（INV-S1）。
	// Storage 关闭后调用返回 ErrClosed。
	NewSnapshot() (Snapshot, error)

	// Close 关闭存储。未关闭的快照与迭代器随之失效。
	Close() error
}

// Reader 是一致性读视图；Storage 与 Snapshot 都实现它。
type Reader interface {
	// Get 返回 key 对应 value 的副本，调用方拥有返回的切片。
	// key 不存在时返回 ErrNotFound。
	Get(key []byte) ([]byte, error)

	// NewIterator 返回范围 [LowerBound, UpperBound) 的前向迭代器。
	// 迭代器按 key 的字节字典序前进（INV-S2），调用方负责 Close。
	NewIterator(opts IterOptions) (Iterator, error)
}

// IterOptions 是迭代范围。len(x)==0 表示该端无界。
type IterOptions struct {
	// LowerBound 是下界，包含；len==0 表示无下界。
	LowerBound []byte
	// UpperBound 是上界，不包含；len==0 表示无上界。
	UpperBound []byte
}

// Iterator 是前向迭代器，不可并发使用。
//
// Key/Value 返回副本，调用方拥有。
// Close 后 Valid() 返回 false，Error() 返回 ErrClosed，其余方法不得 panic。
// 第一版不提供 Prev/Last；新增反向遍历属于 A 层接口变更。
type Iterator interface {
	Valid() bool

	// SeekGE 定位到第一个 >= key 的位置；存在满足条件的项返回 true。
	SeekGE(key []byte) bool

	// Next 前进到下一个位置。
	Next()

	Key() []byte
	Value() []byte
	Error() error
	Close() error
}

// Snapshot 是某一时刻的一致性读视图。
type Snapshot interface {
	Reader

	// Close 释放快照；幂等。
	Close() error
}

// MutationKind 是 mutation 的类型。
type MutationKind uint8

const (
	// Put 写入 key=value（覆盖旧值）。
	Put MutationKind = iota + 1
	// Delete 删除 key；key 不存在不算错误。
	Delete
)

// Mutation 是 Write 的最小单元。
type Mutation struct {
	Kind  MutationKind
	Key   []byte
	Value []byte // 仅 Put 使用；Delete 时忽略
}

// WriteOptions 控制写行为。
type WriteOptions struct {
	// Sync 为 true 时，Write 返回前保证数据已落盘。
	Sync bool
}
