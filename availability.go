package main

// Fixed field names, derived from the same reads as the values. No extra
// filesystem probes or application queries are needed to expose availability.
func memoryAvailability(version int, stat map[string]int64) map[string]bool {
	result := make(map[string]bool, 20)
	keys := [][3]string{
		{"file_bytes", "file", "cache"}, {"anon_bytes", "anon", "rss"},
		{"shmem_bytes", "shmem", "shmem"}, {"slab_bytes", "slab", ""},
		{"dirty_bytes", "file_dirty", "dirty"}, {"writeback_bytes", "file_writeback", "writeback"},
		{"active_file_bytes", "active_file", "active_file"}, {"inactive_file_bytes", "inactive_file", "inactive_file"},
	}
	for _, names := range keys {
		key := names[1]
		if version == 1 {
			key = names[2]
		}
		_, found := stat[key]
		if version == 1 && key != "" {
			_, total := stat["total_"+key]
			found = found || total
		}
		result[names[0]] = key != "" && found
	}
	return result
}
