//go:build amd64

package native

import "embed"

// Assets includes only the selected architecture's static toolchain.
// The shared helper is retained for the development-only external backend.
//
//go:embed assets/tools-linux-amd64.tar.gz assets/libpg18guard.so
var Assets embed.FS
