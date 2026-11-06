// Synthetic read pressure for a disposable lab. Never part of the daemon.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func main() {
	root := flag.String("root", "", "read-only data root")
	rate := flag.Int64("rate", 32<<20, "logical read bytes/sec")
	limit := flag.Int64("bytes", 4<<30, "total read byte limit")
	rounds := flag.Int("rounds", 1, "number of passes")
	flag.Parse()
	if *root == "" || *rate <= 0 || *limit <= 0 || *rounds < 1 {
		fmt.Fprintln(os.Stderr, "root/rate/bytes/rounds required and positive")
		os.Exit(2)
	}
	var paths []string
	err := filepath.WalkDir(*root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	sort.Strings(paths)
	buf := make([]byte, 1<<20)
	started := time.Now()
	var n int64
	var files, failures int
	for round := 0; round < *rounds && n < *limit; round++ {
		for _, p := range paths {
			if n >= *limit {
				break
			}
			f, e := os.Open(p)
			if e != nil {
				failures++
				continue
			}
			files++
			for n < *limit {
				want := int64(len(buf))
				if *limit-n < want {
					want = *limit - n
				}
				got, e := f.Read(buf[:want])
				n += int64(got)
				due := started.Add(time.Duration(float64(n) / float64(*rate) * float64(time.Second)))
				if wait := time.Until(due); wait > 0 {
					time.Sleep(wait)
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					failures++
					break
				}
			}
			f.Close()
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"read_bytes": n, "files": files, "errors": failures, "seconds": time.Since(started).Seconds(), "rate": *rate})
	if failures > 0 {
		os.Exit(1)
	}
}
