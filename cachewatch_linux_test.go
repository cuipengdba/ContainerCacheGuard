//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func coldTestFile(t *testing.T, path string, n int) {
	t.Helper()
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	for i := range buf {
		buf[i] = byte(i)
	}
	for n > 0 {
		k := len(buf)
		if k > n {
			k = n
		}
		if _, e = f.Write(buf[:k]); e != nil {
			t.Fatal(e)
		}
		n -= k
	}
	if e = f.Sync(); e != nil {
		t.Fatal(e)
	}
	if e = adviseDiscard(f, 0, 1<<30); e != nil {
		t.Fatal(e)
	}
}
func residentTest(t *testing.T, path string) ([]byte, uint64) {
	t.Helper()
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		t.Fatal(e)
	}
	b, n, e := residentBitmap(f, s.Size())
	if e != nil {
		t.Fatal(e)
	}
	return b, n
}
func TestNativeWatcherKeepsInitialHotAndDropsColdCopy(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "data")
	os.Mkdir(source, 0700)
	os.Mkdir(filepath.Join(source, "pg_tblspc"), 0700)
	src := filepath.Join(source, "relation")
	coldTestFile(t, src, 16<<20)
	f, e := os.Open(src)
	if e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, 4096)
	if _, e = f.ReadAt(buf, 0); e != nil {
		t.Fatal(e)
	}
	f.Close()
	before, initial := residentTest(t, src)
	if initial == 0 {
		t.Fatal("initial hot page missing")
	}
	dest := filepath.Join(base, "backup")
	w, e := newCacheWatch(context.Background(), Config{PGData: source, FlushBytes: 1 << 20, BitmapBudget: 1 << 20}, dest, true, nil)
	if e != nil {
		t.Fatal(e)
	}
	in, _ := os.Open(src)
	out, _ := os.Create(filepath.Join(dest, "relation"))
	chunk := make([]byte, 64<<10)
	for {
		n, e := in.Read(chunk)
		if n > 0 {
			if _, e := out.Write(chunk[:n]); e != nil {
				t.Fatal(e)
			}
			time.Sleep(time.Millisecond)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	in.Close()
	out.Close()
	if e = w.finish(); e != nil {
		t.Fatal(e)
	}
	after, resident := residentTest(t, src)
	_, written := residentTest(t, filepath.Join(dest, "relation"))
	for i, b := range before {
		if b&after[i] != b {
			t.Fatal("pre-existing hot pages evicted")
		}
	}
	if resident > initial+65536 || written != 0 {
		t.Fatalf("cache retained: initial=%d source=%d destination=%d stats=%+v", initial, resident, written, w.snapshot())
	}
}
func TestNativeIncrementalWatcherMapsBlocksToSource(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "data")
	os.Mkdir(source, 0700)
	os.Mkdir(filepath.Join(source, "pg_tblspc"), 0700)
	src := filepath.Join(source, "relation")
	coldTestFile(t, src, 8<<20)
	dest := filepath.Join(base, "backup")
	w, e := newCacheWatch(context.Background(), Config{PGData: source, FlushBytes: 8192, BitmapBudget: 1 << 20}, dest, true, nil)
	if e != nil {
		t.Fatal(e)
	}
	f, _ := os.Open(src)
	out, _ := os.Create(filepath.Join(dest, "INCREMENTAL.relation"))
	h := make([]byte, 8192)
	blocks := []uint32{10, 100, 200}
	binary.LittleEndian.PutUint32(h, incrementalMagic)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(blocks)))
	for i, b := range blocks {
		binary.LittleEndian.PutUint32(h[12+i*4:], b)
	}
	out.Write(h)
	buf := make([]byte, 8192)
	for _, b := range blocks {
		f.ReadAt(buf, int64(b)*8192)
		out.Write(buf)
		time.Sleep(30 * time.Millisecond)
	}
	f.Close()
	out.Close()
	if e = w.finish(); e != nil {
		t.Fatal(e)
	}
	bits, _ := residentTest(t, src)
	for _, b := range blocks {
		for p := int(b) * 2; p < int(b+1)*2; p++ {
			if bits[p/8]&(1<<uint(p%8)) != 0 {
				t.Fatalf("copied incremental block still resident: %d", b)
			}
		}
	}
	if w.snapshot().IncrementalFiles != 1 || w.snapshot().SourceAdviceBytes != uint64(len(blocks)*8192) {
		t.Fatalf("incremental stats %+v", w.snapshot())
	}
}
func TestIncrementalHeaderRejectsMalformedMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incremental")
	f, _ := os.Create(path)
	defer f.Close()
	h := make([]byte, 8192*3)
	binary.LittleEndian.PutUint32(h, incrementalMagic)
	binary.LittleEndian.PutUint32(h[4:], 2)
	binary.LittleEndian.PutUint32(h[12:], 7)
	binary.LittleEndian.PutUint32(h[16:], 7)
	f.Write(h)
	if _, _, e := incrementalHeader(f, int64(len(h)), true); e == nil {
		t.Fatal("duplicate blocks accepted")
	}
}

func TestIncrementalRangesRespect64KiBPageCompletion(t *testing.T) {
	blocks := []uint32{1, 7, 8, 10, 17}
	got, consumed := completedIncrementalRanges(blocks, 0, 1, 65536)
	if len(got) != 0 || consumed != 0 {
		t.Fatal("page released before its last changed block arrived", got, consumed)
	}
	got, consumed = completedIncrementalRanges(blocks, 0, 3, 65536)
	if len(got) != 1 || got[0] != [2]int64{0, 65536} || consumed != 2 {
		t.Fatal("incorrect completed page", got, consumed)
	}
	got, consumed = completedIncrementalRanges(blocks, consumed, 5, 65536)
	if len(got) != 2 || got[0] != [2]int64{65536, 131072} || got[1] != [2]int64{131072, 196608} || consumed != 5 {
		t.Fatal("incorrect remaining page groups", got, consumed)
	}
}

func TestNoFollowRejectsParentSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if e := os.Mkdir(real, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(real, "file"), []byte("data"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(real, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if f, e := fileOpenNoFollow(filepath.Join(root, "link", "file")); e == nil {
		f.Close()
		t.Fatal("parent symlink was followed")
	}
}
