package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// pgBackRest 2.60 verify can report invalid files without returning a nonzero
// process exit code. Always parse its explicit verification result as well.
func (r *Runner) verify(label string) error {
	args := []string{"--output=text", "--verbose", "--log-level-console=info"}
	if label != "" {
		args = append(args, "--set="+label)
	}
	var out cappedBuffer
	out.limit = 16 << 20
	if e := r.engine("verify", args, &out, nil); e != nil {
		return e
	}
	if e := validVerification(out.String()); e != nil {
		return e
	}
	event("verification_passed", map[string]any{"backup_label": label})
	return nil
}
func validVerification(text string) error {
	ok, backup := false, false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "status: ok" {
			ok = true
		}
		if strings.HasPrefix(line, "status:") && line != "status: ok" {
			return errors.New("repository verification reports an error")
		}
		if strings.HasPrefix(line, "backup:") {
			if !strings.Contains(line, ", status: valid") {
				return errors.New("repository verification found an invalid, missing or incomplete backup")
			}
			backup = true
		}
	}
	if !ok || !backup {
		return errors.New("missing explicit successful backup verification result")
	}
	return nil
}

// A restore is admitted only for a known label from a PostgreSQL 18 database
// identity. Numeric IDs are decoded as integers, never floating-point values.
func (r *Runner) backupLabel(requested string) (string, error) {
	var out cappedBuffer
	out.limit = 16 << 20
	if e := r.engine("info", []string{"--output=json", "--log-level-console=off"}, &out, nil); e != nil {
		return "", e
	}
	return selectBackup(out.Bytes(), requested)
}
func selectBackup(b []byte, requested string) (string, error) {
	var stanzas []struct {
		Name string `json:"name"`
		DB   []struct {
			ID      int    `json:"id"`
			Version string `json:"version"`
			RepoKey int    `json:"repo-key"`
		} `json:"db"`
		Backup []struct {
			Label    string `json:"label"`
			Error    bool   `json:"error"`
			Database struct {
				ID      int `json:"id"`
				RepoKey int `json:"repo-key"`
			} `json:"database"`
		} `json:"backup"`
	}
	if e := json.Unmarshal(b, &stanzas); e != nil {
		return "", fmt.Errorf("repository info: %w", e)
	}
	if len(stanzas) != 1 {
		return "", errors.New("expected exactly one stanza")
	}
	s := stanzas[0]
	for i := len(s.Backup) - 1; i >= 0; i-- {
		v := s.Backup[i]
		if requested != "" && v.Label != requested {
			continue
		}
		if v.Error {
			if requested != "" {
				return "", errors.New("selected backup has recorded errors")
			}
			continue
		}
		for _, db := range s.DB {
			if db.ID == v.Database.ID && db.RepoKey == v.Database.RepoKey {
				if db.Version != "18" {
					return "", errors.New("selected backup is not PostgreSQL 18")
				}
				return v.Label, nil
			}
		}
		return "", errors.New("backup database identity not found")
	}
	return "", errors.New("no matching error-free backup found")
}
