// Synthetic read pressure for a disposable lab. Never part of the daemon.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

func main() {
	root := flag.String("root", "", "read-only data root")
	rate := flag.Int64("rate", 32<<20, "logical read bytes/sec")
	limit := flag.Int64("bytes", 4<<30, "total read byte limit")
	rounds := flag.Int("rounds", 1, "number of passes")
	working := flag.Int64("working-set-bytes", 0, "maximum bytes per pass; 0 means full root")
	hotPath := flag.String("hot-path", "", "optional hot file for concurrent 8KiB random reads")
	flag.Parse()
	if *root == "" || *rate <= 0 || *limit <= 0 || *rounds < 1 || *working < 0 {
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
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var latencies []float64
	var hotErrors int
	if *hotPath != "" {
		f, err := os.Open(*hotPath)
		if err != nil {
			panic(err)
		}
		st, err := f.Stat()
		if err != nil || st.Size() < 8192 {
			panic("hot file too small")
		}
		span := st.Size()
		if span > 8<<20 {
			span = 8 << 20
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer f.Close()
			r := rand.New(rand.NewSource(1))
			buf := make([]byte, 8192)
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					off := r.Int63n(span/8192) * 8192
					start := time.Now()
					_, err := f.ReadAt(buf, off)
					if err != nil {
						hotErrors++
					} else if len(latencies) < 1000000 {
						latencies = append(latencies, float64(time.Since(start).Nanoseconds())/1e6)
					}
				}
			}
		}()
	}
	for round := 0; round < *rounds && n < *limit; round++ {
		passStart := n
		for _, p := range paths {
			if n >= *limit || (*working > 0 && n-passStart >= *working) {
				break
			}
			f, e := os.Open(p)
			if e != nil {
				failures++
				continue
			}
			files++
			for n < *limit && (*working == 0 || n-passStart < *working) {
				want := int64(len(buf))
				if *limit-n < want {
					want = *limit - n
				}
				if *working > 0 && *working-(n-passStart) < want {
					want = *working - (n - passStart)
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
	close(stop)
	wg.Wait()
	sort.Float64s(latencies)
	quantile := func(p float64) any {
		if len(latencies) == 0 {
			return nil
		}
		return latencies[int(float64(len(latencies)-1)*p)]
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"read_bytes": n, "files": files, "errors": failures, "seconds": time.Since(started).Seconds(), "rate": *rate, "hot_samples": len(latencies), "hot_errors": hotErrors, "hot_p50_ms": quantile(.5), "hot_p95_ms": quantile(.95), "hot_p99_ms": quantile(.99), "working_set_bytes": *working})
	if failures > 0 || hotErrors > 0 || n < *limit {
		os.Exit(1)
	}
}
