//go:build !amd64 && !arm64

package native

import "embed"

// Unsupported architectures have no runnable client bundle.
var Assets embed.FS
