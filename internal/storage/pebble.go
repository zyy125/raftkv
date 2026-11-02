package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cockroachdb/pebble/v2"
)

// pebbleStore 是 Storage 的 Pebble 实现。
//
// 并发模型：
//   - 读、写、迭代操作持 RLock，且只在单次方法调用期间持有；
//   - Close、NewIterator、NewSnapshot、Iterator.Close、Snapshot.Close 持 Lock，
//     因为它们要修改关闭状态或资源登记表；
//   - Close 在持 Lock 期间先关闭所有未释放的迭代器与快照，再关闭 DB，
//     满足 Pebble“所有迭代器关闭后才能 Close DB”的要求。
type pebbleStore struct {
	db *pebble.DB

	mu        sync.RWMutex
	closed    bool
	iterators map[*pebbleIterator]struct{}
	snapshots map[*pebbleSnapshot]struct{}
}

var _ Storage = (*pebbleStore)(nil)

// Open 打开（不存在则创建）opts.Path 下的存储。
func Open(opts Options) (Storage, error) {
	if opts.Path == "" {
		return nil, errors.New("storage: path must not be empty")
	}
	db, err := pebble.Open(opts.Path, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", opts.Path, err)
	}
	return &pebbleStore{
		db:        db,
		iterators: make(map[*pebbleIterator]struct{}),
		snapshots: make(map[*pebbleSnapshot]struct{}),
	}, nil
}

func (s *pebbleStore) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	return pebbleGet(s.db, key)
}

func (s *pebbleStore) Write(muts []Mutation, opts WriteOptions) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	for _, m := range muts {
		if m.Kind != Put && m.Kind != Delete {
			return ErrInvalidMutation
		}
	}
	if len(muts) == 0 {
		return nil
	}

	b := s.db.NewBatch()
	for _, m := range muts {
		var err error
		switch m.Kind {
		case Put:
			err = b.Set(m.Key, m.Value, nil)
		case Delete:
			err = b.Delete(m.Key, nil)
		}
		if err != nil {
			return fmt.Errorf("storage: write: %w", errors.Join(err, b.Close()))
		}
	}
	applyErr := s.db.Apply(b, &pebble.WriteOptions{Sync: opts.Sync})
	closeErr := b.Close()
	if applyErr != nil {
		return fmt.Errorf("storage: write: %w", applyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("storage: write close batch: %w", closeErr)
	}
	return nil
}

func (s *pebbleStore) NewIterator(opts IterOptions) (Iterator, error) {
	lo, hi := copyBounds(opts)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if lo != nil && hi != nil && bytes.Compare(lo, hi) >= 0 {
		return &emptyIterator{store: s}, nil
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, fmt.Errorf("storage: new iterator: %w", err)
	}
	wi := &pebbleIterator{store: s, it: it}
	s.iterators[wi] = struct{}{}
	return wi, nil
}

func (s *pebbleStore) NewSnapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	snap := &pebbleSnapshot{store: s, snap: s.db.NewSnapshot()}
	s.snapshots[snap] = struct{}{}
	return snap, nil
}

func (s *pebbleStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error
	for it := range s.iterators {
		it.closed = true
		if err := it.it.Close(); err != nil {
			errs = append(errs, fmt.Errorf("storage: close iterator: %w", err))
		}
	}
	clear(s.iterators)
	for snap := range s.snapshots {
		snap.closed = true
		if err := snap.snap.Close(); err != nil {
			errs = append(errs, fmt.Errorf("storage: close snapshot: %w", err))
		}
	}
	clear(s.snapshots)
	if err := s.db.Close(); err != nil {
		errs = append(errs, fmt.Errorf("storage: close db: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// pebbleGetter 同时由 *pebble.DB 与 *pebble.Snapshot 实现。
type pebbleGetter interface {
	Get(key []byte) (value []byte, closer io.Closer, err error)
}

func pebbleGet(r pebbleGetter, key []byte) ([]byte, error) {
	value, closer, err := r.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: get: %w", err)
	}
	out := cloneBytes(value)
	if closer != nil {
		if err := closer.Close(); err != nil {
			return nil, fmt.Errorf("storage: get close: %w", err)
		}
	}
	return out, nil
}

func copyBounds(opts IterOptions) (lo, hi []byte) {
	if len(opts.LowerBound) > 0 {
		lo = cloneBytes(opts.LowerBound)
	}
	if len(opts.UpperBound) > 0 {
		hi = cloneBytes(opts.UpperBound)
	}
	return lo, hi
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// pebbleIterator 是 Iterator 的 Pebble 实现，不保证并发安全。
type pebbleIterator struct {
	store  *pebbleStore
	it     *pebble.Iterator
	closed bool
}

var _ Iterator = (*pebbleIterator)(nil)

func (i *pebbleIterator) Valid() bool {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	return !i.closed && !i.store.closed && i.it.Valid()
}

func (i *pebbleIterator) SeekGE(key []byte) bool {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed {
		return false
	}
	return i.it.SeekGE(key)
}

func (i *pebbleIterator) Next() {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed {
		return
	}
	i.it.Next()
}

func (i *pebbleIterator) Key() []byte {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed || !i.it.Valid() {
		return nil
	}
	return cloneBytes(i.it.Key())
}

func (i *pebbleIterator) Value() []byte {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed || !i.it.Valid() {
		return nil
	}
	return cloneBytes(i.it.Value())
}

func (i *pebbleIterator) Error() error {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed {
		return ErrClosed
	}
	if err := i.it.Error(); err != nil {
		return fmt.Errorf("storage: iterate: %w", err)
	}
	return nil
}

func (i *pebbleIterator) Close() error {
	i.store.mu.Lock()
	defer i.store.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	delete(i.store.iterators, i)
	if i.store.closed {
		return nil
	}
	if err := i.it.Close(); err != nil {
		return fmt.Errorf("storage: close iterator: %w", err)
	}
	return nil
}

// emptyIterator 用于 lower >= upper 的空范围；不持有任何 Pebble 资源。
type emptyIterator struct {
	store  *pebbleStore
	closed bool
}

var _ Iterator = (*emptyIterator)(nil)

func (i *emptyIterator) Valid() bool        { return false }
func (i *emptyIterator) SeekGE([]byte) bool { return false }
func (i *emptyIterator) Next()              {}
func (i *emptyIterator) Key() []byte        { return nil }
func (i *emptyIterator) Value() []byte      { return nil }

func (i *emptyIterator) Error() error {
	i.store.mu.RLock()
	defer i.store.mu.RUnlock()
	if i.closed || i.store.closed {
		return ErrClosed
	}
	return nil
}

func (i *emptyIterator) Close() error {
	i.store.mu.Lock()
	defer i.store.mu.Unlock()
	i.closed = true
	return nil
}

// pebbleSnapshot 是 Snapshot 的 Pebble 实现。
type pebbleSnapshot struct {
	store  *pebbleStore
	snap   *pebble.Snapshot
	closed bool
}

var _ Snapshot = (*pebbleSnapshot)(nil)

func (s *pebbleSnapshot) Get(key []byte) ([]byte, error) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if s.closed || s.store.closed {
		return nil, ErrClosed
	}
	return pebbleGet(s.snap, key)
}

func (s *pebbleSnapshot) NewIterator(opts IterOptions) (Iterator, error) {
	lo, hi := copyBounds(opts)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.closed || s.store.closed {
		return nil, ErrClosed
	}
	if lo != nil && hi != nil && bytes.Compare(lo, hi) >= 0 {
		return &emptyIterator{store: s.store}, nil
	}
	it, err := s.snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, fmt.Errorf("storage: new iterator: %w", err)
	}
	wi := &pebbleIterator{store: s.store, it: it}
	s.store.iterators[wi] = struct{}{}
	return wi, nil
}

func (s *pebbleSnapshot) Close() error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	delete(s.store.snapshots, s)
	if s.store.closed {
		return nil
	}
	if err := s.snap.Close(); err != nil {
		return fmt.Errorf("storage: close snapshot: %w", err)
	}
	return nil
}
