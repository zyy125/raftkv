package storage_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zhuying/raftkv/internal/storage"
)

func openStore(t *testing.T, path string) storage.Storage {
	t.Helper()
	s, err := storage.Open(storage.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func put(k, v string) storage.Mutation {
	return storage.Mutation{Kind: storage.Put, Key: []byte(k), Value: []byte(v)}
}

func write(t *testing.T, s storage.Storage, muts ...storage.Mutation) {
	t.Helper()
	if err := s.Write(muts, storage.WriteOptions{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func mustGet(t *testing.T, r storage.Reader, key string) string {
	t.Helper()
	v, err := r.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return string(v)
}

func scan(t *testing.T, r storage.Reader, opts storage.IterOptions) (keys, vals []string) {
	t.Helper()
	it, err := r.NewIterator(opts)
	if err != nil {
		t.Fatalf("NewIterator: %v", err)
	}
	defer func() {
		if err := it.Close(); err != nil {
			t.Errorf("Iterator.Close: %v", err)
		}
	}()
	for ok := it.SeekGE(opts.LowerBound); ok; ok = it.Valid() {
		keys = append(keys, string(it.Key()))
		vals = append(vals, string(it.Value()))
		it.Next()
	}
	if err := it.Error(); err != nil {
		t.Fatalf("Iterator.Error: %v", err)
	}
	return keys, vals
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := storage.Open(storage.Options{}); err == nil {
		t.Fatal("Open with empty path: got nil error, want error")
	}
}

func TestWriteGetDelete(t *testing.T) {
	s := openStore(t, t.TempDir())

	write(t, s, put("a", "1"), put("b", "2"))
	if got := mustGet(t, s, "a"); got != "1" {
		t.Fatalf("Get(a) = %q, want %q", got, "1")
	}
	if got := mustGet(t, s, "b"); got != "2" {
		t.Fatalf("Get(b) = %q, want %q", got, "2")
	}

	if _, err := s.Get([]byte("missing")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}

	write(t, s, put("a", "3"))
	if got := mustGet(t, s, "a"); got != "3" {
		t.Fatalf("Get(a) after overwrite = %q, want %q", got, "3")
	}

	write(t, s, storage.Mutation{Kind: storage.Delete, Key: []byte("b")})
	if _, err := s.Get([]byte("b")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get(b) after delete error = %v, want ErrNotFound", err)
	}

	// 删除不存在的 key 不是错误。
	write(t, s, storage.Mutation{Kind: storage.Delete, Key: []byte("missing")})

	// 空批是合法 no-op。
	if err := s.Write(nil, storage.WriteOptions{}); err != nil {
		t.Fatalf("Write(nil) error = %v, want nil", err)
	}

	// 空 value 与“不存在”必须可区分。
	write(t, s, put("empty", ""))
	v, err := s.Get([]byte("empty"))
	if err != nil {
		t.Fatalf("Get(empty) error = %v, want nil", err)
	}
	if len(v) != 0 {
		t.Fatalf("Get(empty) = %q, want empty", v)
	}
}

func TestWriteInvalidMutationIsAtomic(t *testing.T) {
	s := openStore(t, t.TempDir())

	err := s.Write([]storage.Mutation{
		put("x", "1"),
		{Kind: storage.MutationKind(99), Key: []byte("y"), Value: []byte("2")},
	}, storage.WriteOptions{})
	if !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("Write with invalid kind error = %v, want ErrInvalidMutation", err)
	}
	if _, err := s.Get([]byte("x")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get(x) after failed batch error = %v, want ErrNotFound (batch must not partially apply)", err)
	}
}

func TestIteratorBounds(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"), put("b", "2"), put("c", "3"), put("d", "4"))

	keys, vals := scan(t, s, storage.IterOptions{
		LowerBound: []byte("a"),
		UpperBound: []byte("c"),
	})
	if !equal(keys, []string{"a", "b"}) {
		t.Fatalf("keys in [a,c) = %v, want [a b]", keys)
	}
	if !equal(vals, []string{"1", "2"}) {
		t.Fatalf("values in [a,c) = %v, want [1 2]", vals)
	}

	// 上界 key 本身必须被排除。
	keys, _ = scan(t, s, storage.IterOptions{
		LowerBound: []byte("c"),
		UpperBound: []byte("d"),
	})
	if !equal(keys, []string{"c"}) {
		t.Fatalf("keys in [c,d) = %v, want [c]", keys)
	}

	keys, _ = scan(t, s, storage.IterOptions{})
	if !equal(keys, []string{"a", "b", "c", "d"}) {
		t.Fatalf("unbounded keys = %v, want [a b c d]", keys)
	}
}

func TestIteratorEmptyRange(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"), put("b", "2"))

	for _, opts := range []storage.IterOptions{
		{LowerBound: []byte("b"), UpperBound: []byte("b")},
		{LowerBound: []byte("c"), UpperBound: []byte("b")},
	} {
		keys, _ := scan(t, s, opts)
		if len(keys) != 0 {
			t.Fatalf("keys for empty range %q..%q = %v, want none", opts.LowerBound, opts.UpperBound, keys)
		}
	}
}

func TestIteratorByteOrderBounds(t *testing.T) {
	s := openStore(t, t.TempDir())
	k1 := []byte{0x01, 0xff}
	k2 := []byte{0x02, 0x00}
	write(t, s,
		storage.Mutation{Kind: storage.Put, Key: k1, Value: []byte("one")},
		storage.Mutation{Kind: storage.Put, Key: k2, Value: []byte("two")},
	)

	keys, _ := scan(t, s, storage.IterOptions{
		LowerBound: []byte{0x01},
		UpperBound: []byte{0x02},
	})
	if len(keys) != 1 || keys[0] != string(k1) {
		t.Fatalf("keys in [0x01,0x02) = %q, want [%q]", keys, k1)
	}
}

func TestIteratorCopiesBounds(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"), put("b", "2"))

	lo := []byte("a")
	hi := []byte("c")
	it, err := s.NewIterator(storage.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		t.Fatalf("NewIterator: %v", err)
	}
	defer it.Close()

	// 调用方修改传入的切片，不应影响已经创建的迭代器。
	lo[0] = 'z'
	hi[0] = 'z'

	var keys []string
	for ok := it.SeekGE(nil); ok; ok = it.Valid() {
		keys = append(keys, string(it.Key()))
		it.Next()
	}
	if !equal(keys, []string{"a", "b"}) {
		t.Fatalf("keys after mutating bounds = %v, want [a b]", keys)
	}
}

func TestIteratorReturnsCopies(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"))

	it, err := s.NewIterator(storage.IterOptions{})
	if err != nil {
		t.Fatalf("NewIterator: %v", err)
	}
	defer it.Close()

	if !it.SeekGE([]byte("a")) {
		t.Fatal("SeekGE(a) = false, want true")
	}
	k := it.Key()
	v := it.Value()
	it.Next()

	if string(k) != "a" || string(v) != "1" {
		t.Fatalf("saved key/value after Next = (%q,%q), want (a,1)", k, v)
	}
	v[0] = '9'
	if got := mustGet(t, s, "a"); got != "1" {
		t.Fatalf("mutating returned value affected store: Get(a) = %q, want 1", got)
	}
}

func TestIteratorClose(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"))

	it, err := s.NewIterator(storage.IterOptions{})
	if err != nil {
		t.Fatalf("NewIterator: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
	if it.Valid() {
		t.Fatal("Valid after Close = true, want false")
	}
	if !errors.Is(it.Error(), storage.ErrClosed) {
		t.Fatalf("Error after Close = %v, want ErrClosed", it.Error())
	}
	it.Next()
	if it.SeekGE(nil) {
		t.Fatal("SeekGE after Close = true, want false")
	}
	if it.Key() != nil || it.Value() != nil {
		t.Fatal("Key/Value after Close != nil")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"))

	snap, err := s.NewSnapshot()
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	defer snap.Close()

	write(t, s, put("a", "2"), put("b", "2"))

	if got := mustGet(t, snap, "a"); got != "1" {
		t.Fatalf("snapshot Get(a) = %q, want 1 (snapshot must isolate later writes)", got)
	}
	if _, err := snap.Get([]byte("b")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("snapshot Get(b) error = %v, want ErrNotFound", err)
	}
	if got := mustGet(t, s, "a"); got != "2" {
		t.Fatalf("store Get(a) = %q, want 2", got)
	}

	keys, vals := scan(t, snap, storage.IterOptions{})
	if !equal(keys, []string{"a"}) || !equal(vals, []string{"1"}) {
		t.Fatalf("snapshot scan = (%v,%v), want ([a],[1])", keys, vals)
	}
}

func TestSnapshotClose(t *testing.T) {
	s := openStore(t, t.TempDir())
	write(t, s, put("a", "1"))

	snap, err := s.NewSnapshot()
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
	if _, err := snap.Get([]byte("a")); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("Get after Close error = %v, want ErrClosed", err)
	}
	if _, err := snap.NewIterator(storage.IterOptions{}); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("NewIterator after Close error = %v, want ErrClosed", err)
	}
}

func TestCloseWithOutstandingResources(t *testing.T) {
	s, err := storage.Open(storage.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	write(t, s, put("a", "1"))

	it, err := s.NewIterator(storage.IterOptions{})
	if err != nil {
		t.Fatalf("NewIterator: %v", err)
	}
	if !it.SeekGE(nil) {
		t.Fatal("SeekGE = false, want true")
	}
	snap, err := s.NewSnapshot()
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}

	if _, err := s.Get([]byte("a")); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("Get after Close error = %v, want ErrClosed", err)
	}
	if err := s.Write(nil, storage.WriteOptions{}); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("Write after Close error = %v, want ErrClosed", err)
	}
	if _, err := s.NewIterator(storage.IterOptions{}); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("NewIterator after Close error = %v, want ErrClosed", err)
	}
	if _, err := s.NewSnapshot(); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("NewSnapshot after Close error = %v, want ErrClosed", err)
	}

	if it.Valid() {
		t.Fatal("outstanding iterator Valid after store Close = true, want false")
	}
	if !errors.Is(it.Error(), storage.ErrClosed) {
		t.Fatalf("outstanding iterator Error after store Close = %v, want ErrClosed", it.Error())
	}
	if err := it.Close(); err != nil {
		t.Fatalf("outstanding iterator Close after store Close: %v", err)
	}
	if _, err := snap.Get([]byte("a")); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("outstanding snapshot Get after store Close error = %v, want ErrClosed", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatalf("outstanding snapshot Close after store Close: %v", err)
	}
}

func TestPersistence(t *testing.T) {
	path := t.TempDir()

	s, err := storage.Open(storage.Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Write([]storage.Mutation{put("k", "v")}, storage.WriteOptions{Sync: true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := storage.Open(storage.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if got := mustGet(t, s2, "k"); got != "v" {
		t.Fatalf("Get(k) after reopen = %q, want v", got)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	s := openStore(t, t.TempDir())

	const writers, perWriter = 4, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				k := fmt.Sprintf("k-%02d-%03d", w, i)
				err := s.Write([]storage.Mutation{{
					Kind:  storage.Put,
					Key:   []byte(k),
					Value: []byte(k),
				}}, storage.WriteOptions{})
				if err != nil {
					t.Errorf("Write(%s): %v", k, err)
				}
			}
		}(w)
	}
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				it, err := s.NewIterator(storage.IterOptions{})
				if err != nil {
					t.Errorf("NewIterator: %v", err)
					return
				}
				for ok := it.SeekGE(nil); ok; ok = it.Valid() {
					_ = it.Key()
					_ = it.Value()
					it.Next()
				}
				if err := it.Error(); err != nil {
					t.Errorf("Iterator.Error: %v", err)
				}
				if err := it.Close(); err != nil {
					t.Errorf("Iterator.Close: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	keys, _ := scan(t, s, storage.IterOptions{})
	if len(keys) != writers*perWriter {
		t.Fatalf("got %d keys, want %d", len(keys), writers*perWriter)
	}
}
