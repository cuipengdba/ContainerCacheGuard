package main

import (
	"errors"
	"fmt"
	"strings"
)

type baseOptions struct {
	destination, manifest, host, user, port, rate, label    string
	format, target, walMethod, compression, connstr, waldir string
	manifestChecksums                                       string
	tablespaces                                             []string
	progress, verbose, noManifest, noSync                   bool
}

type pgOption struct {
	short byte
	value bool
}

// PostgreSQL 18.6's complete pg_basebackup getopt_long table. The original
// argv is retained; this scanner only identifies paths and guard coverage.
var baseOptionSpec = map[string]pgOption{
	"help": {'?', false}, "version": {'V', false}, "pgdata": {'D', true},
	"format": {'F', true}, "incremental": {'i', true}, "checkpoint": {'c', true},
	"create-slot": {'C', false}, "max-rate": {'r', true}, "write-recovery-conf": {'R', false},
	"slot": {'S', true}, "target": {'t', true}, "tablespace-mapping": {'T', true},
	"wal-method": {'X', true}, "gzip": {'z', false}, "compress": {'Z', true},
	"label": {'l', true}, "no-clean": {'n', false}, "no-sync": {'N', false},
	"dbname": {'d', true}, "host": {'h', true}, "port": {'p', true}, "username": {'U', true},
	"no-password": {'w', false}, "password": {'W', false}, "status-interval": {'s', true},
	"verbose": {'v', false}, "progress": {'P', false}, "waldir": {0, true},
	"no-slot": {0, false}, "no-verify-checksums": {0, false}, "no-estimate-size": {0, false},
	"no-manifest": {0, false}, "manifest-force-encode": {0, false},
	"manifest-checksums": {0, true}, "sync-method": {0, true},
}

func parseBaseOptions(args []string) (baseOptions, error) {
	o := baseOptions{format: "plain", target: "client", walMethod: "stream", manifestChecksums: "CRC32C"}
	short := map[byte]string{}
	for name, spec := range baseOptionSpec {
		if spec.short != 0 {
			short[spec.short] = name
		}
	}
	apply := func(name, value string) {
		switch name {
		case "pgdata":
			o.destination = value
		case "incremental":
			o.manifest = value
		case "host":
			o.host = value
		case "port":
			o.port = value
		case "username":
			o.user = value
		case "max-rate":
			o.rate = value
		case "label":
			o.label = value
		case "format":
			o.format = value
		case "target":
			o.target = value
		case "wal-method":
			o.walMethod = value
		case "compress":
			o.compression = value
		case "gzip":
			o.compression = "gzip"
		case "dbname":
			o.connstr = value
		case "waldir":
			o.waldir = value
		case "tablespace-mapping":
			o.tablespaces = append(o.tablespaces, value)
		case "progress":
			o.progress = true
		case "verbose":
			o.verbose = true
		case "no-manifest":
			o.noManifest = true
		case "no-sync":
			o.noSync = true
		case "manifest-checksums":
			o.manifestChecksums = value
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i != len(args)-1 {
				return o, errors.New("pg_basebackup has no positional arguments")
			}
			break
		}
		if strings.HasPrefix(a, "--") {
			name, value, has := strings.Cut(a[2:], "=")
			spec, ok := baseOptionSpec[name]
			if !ok {
				match := ""
				for candidate := range baseOptionSpec {
					if strings.HasPrefix(candidate, name) {
						if match != "" {
							return o, fmt.Errorf("ambiguous PostgreSQL option --%s", name)
						}
						match = candidate
					}
				}
				if match == "" {
					return o, fmt.Errorf("unknown PostgreSQL option --%s", name)
				}
				name, spec = match, baseOptionSpec[match]
			}
			if spec.value && !has {
				i++
				if i >= len(args) {
					return o, fmt.Errorf("--%s requires a value", name)
				}
				value = args[i]
			}
			if !spec.value && has {
				return o, fmt.Errorf("--%s takes no value", name)
			}
			apply(name, value)
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			return o, errors.New("pg_basebackup has no positional arguments")
		}
		for j := 1; j < len(a); j++ {
			name, ok := short[a[j]]
			if !ok {
				return o, fmt.Errorf("unknown PostgreSQL option -%c", a[j])
			}
			spec := baseOptionSpec[name]
			value := ""
			if spec.value {
				if j+1 < len(a) {
					value = a[j+1:]
				} else {
					i++
					if i >= len(args) {
						return o, fmt.Errorf("-%c requires a value", a[j])
					}
					value = args[i]
				}
				apply(name, value)
				break
			}
			apply(name, value)
		}
	}
	if o.destination == "" && o.target == "client" {
		return o, errors.New("-D/--pgdata is required for a client backup")
	}
	return o, nil
}
