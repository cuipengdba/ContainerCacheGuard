package main

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestBaseOptionsEveryUpstreamOptionAndRawArguments(t *testing.T) {
	// The scanner must accept the complete upstream option table, including
	// options which do not apply together. Upstream checks semantic conflicts.
	for name, spec := range baseOptionSpec {
		if name == "help" || name == "version" {
			continue
		}
		value := "value"
		switch name {
		case "pgdata":
			value = "/backup/test"
		case "target":
			value = "server:/server/output"
		}
		forms := [][]string{{"--" + name}}
		if spec.value {
			forms = [][]string{{"--" + name, value}, {"--" + name + "=" + value}}
		}
		if spec.short != 0 {
			form := []string{"-" + string(spec.short)}
			if spec.value {
				form = append(form, value)
				forms = append(forms, []string{"-" + string(spec.short) + value})
			}
			forms = append(forms, form)
		}
		for _, form := range forms {
			a := append([]string{"-D", "/backup/test"}, form...)
			before := append([]string{}, a...)
			if _, err := parseBaseOptions(a); err != nil {
				t.Errorf("%s: %v", strings.Join(a, " "), err)
			}
			if !reflect.DeepEqual(a, before) {
				t.Fatal("scanner changed native argv")
			}
		}
	}
}

func TestBaseOptionsClustersAbbreviationsAndOverrides(t *testing.T) {
	o, err := parseBaseOptions([]string{"-D/backup/a", "-PvFp", "-Xstream", "--manifest-checksums=SHA256", "--pgdat=/backup/b", "-r8M", "--"})
	if err != nil {
		t.Fatal(err)
	}
	if o.destination != "/backup/b" || !o.progress || !o.verbose || o.format != "p" || o.walMethod != "stream" || o.manifestChecksums != "SHA256" || o.rate != "8M" {
		t.Fatalf("incorrect scan: %+v", o)
	}
	for _, args := range [][]string{{"-D", "/backup/a", "--man=x"}, {"-D"}, {"-D/a", "--progress=x"}, {"-D/a", "-q"}, {"-D/a", "--", "trailing"}, {"-D/a", "--unknown"}} {
		if _, err := parseBaseOptions(args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestTablespaceMappingEscapedEquals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux absolute tablespace paths")
	}
	old, target, err := parseTablespaceMapping(`/old\=one=/new\=two`)
	if err != nil || old != "/old=one" || target != "/new=two" {
		t.Fatal(old, target, err)
	}
	for _, value := range []string{"relative=/absolute", "/a=/b=/c", "/only"} {
		if _, _, err := parseTablespaceMapping(value); err == nil {
			t.Fatal("accepted invalid mapping", value)
		}
	}
}
