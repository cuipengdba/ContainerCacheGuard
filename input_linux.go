//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// Match encoding/json's Unicode case folding, so aliases such as TIME/time
// cannot evade duplicate-key detection. Maps are bounded by input size.
func foldedKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		first, best := r, r
		for n := unicode.SimpleFold(r); n != first; n = unicode.SimpleFold(n) {
			if n < best {
				best = n
			}
		}
		b.WriteRune(best)
	}
	return b.String()
}
func uniqueJSONObject(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON嵌套超过32层")
		}
		token, e := d.Token()
		if e != nil {
			return e
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := key.(string)
				if !ok {
					return errors.New("JSON键无效")
				}
				folded := foldedKey(s)
				if seen[folded] {
					return errors.New("JSON字段重复（含大小写别名）")
				}
				seen[folded] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errors.New("JSON容器无效")
		}
		_, e = d.Token()
		return e
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON顶层必须为对象")
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("JSON尾部含多余数据")
	}
	return nil
}
func readRegularJSON(path string, limit int64, out any, strict bool) error {
	f, e := safeOpen(path)
	if e != nil {
		return e
	}
	defer f.Close()
	id, e := statFile(f)
	if e != nil || id.Mode&0170000 != 0100000 {
		return errors.New("信号必须为真实普通文件")
	}
	body, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return e
	}
	if int64(len(body)) > limit {
		return errors.New("JSON超过大小限制")
	}
	if e = uniqueJSONObject(body); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if strict {
		d.DisallowUnknownFields()
	}
	return d.Decode(out)
}
