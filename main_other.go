//go:build !linux || (!amd64 && !arm64)

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ContainerCacheGuard runtime supports Linux amd64/arm64 only")
	os.Exit(1)
}
