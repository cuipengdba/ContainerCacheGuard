# Third-party components

pgcacheguard's own controller and I/O adapter use the MIT license. The embedded
client programs are modified builds of PostgreSQL 18.6, with directly linked
cache/rate handling and a basebackup connection identity check. They retain the
PostgreSQL license. These client modifications do not alter the database server.

| Component | Version | License / included notice | Upstream |
| --- | --- | --- | --- |
| PostgreSQL clients | 18.6 | PostgreSQL License, `third_party/licenses/postgresql.txt` | https://www.postgresql.org/ |
| musl libc | 1.2.6 | MIT and notices in `third_party/licenses/musl.txt` | https://musl.libc.org/ |
| OpenSSL | 3.5.9 | Apache License 2.0, `third_party/licenses/openssl.txt` | https://openssl.org/ |
| zlib | 1.4 | zlib license, `third_party/licenses/zlib.txt` | https://zlib.net/ |
| LZ4 library | 1.11.0 | BSD 2-Clause, `third_party/licenses/lz4.txt` | https://github.com/lz4/lz4 |
| Zstandard library | 1.5.7 | BSD license option, `third_party/licenses/zstd.txt` | https://github.com/facebook/zstd |
| Go runtime | Build-dependent; recorded by `go version -m` | BSD 3-Clause and `third_party/licenses/go.txt` | https://go.dev/ |

Pinned archive URLs and SHA256 values are in `scripts/sources.json`. Build
recipes download and verify the upstream sources, then apply the client changes
from `native/guard.c` and `native/basebackup-identity.inc`. The bundled build
enables TLS, gzip, LZ4 and Zstandard; GSSAPI, LDAP and OAuth/libcurl are not enabled.

The optional development shared adapter is not used by the embedded static
backend. Database server extensions, business data, QEMU, Zig, and private lab
images are not part of the distributed runtime. ARM64 emulation used during lab
testing is separate from the released executable and its license inventory.
