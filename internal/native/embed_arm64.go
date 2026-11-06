//go:build arm64

package native

import "embed"

//go:embed assets/tools-linux-arm64.tar.gz
var Assets embed.FS
