package main

import "testing"

func TestMemoryAvailabilityTracksFieldsAndV1Fallback(t *testing.T) {
	v2 := memoryAvailability(2, map[string]int64{"file": 0, "anon": 20})
	if !v2["file_bytes"] || !v2["anon_bytes"] || v2["shmem_bytes"] || v2["slab_bytes"] {
		t.Fatal("zero field and absent field were confused", v2)
	}
	v1 := memoryAvailability(1, map[string]int64{"total_cache": 30, "rss": 40, "total_dirty": 0, "slab": 10})
	if !v1["file_bytes"] || !v1["anon_bytes"] || !v1["dirty_bytes"] || v1["slab_bytes"] || v1["shmem_bytes"] {
		t.Fatal("v1 totals/fallback or unsupported slab were misreported", v1)
	}
}
