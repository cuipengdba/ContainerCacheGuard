package main

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata" // Named zones also work in a minimal static-binary container.
)

func (c Config) logDirectory() string {
	if c.LogDirectory != "" {
		return c.LogDirectory
	}
	base := strings.TrimSuffix(filepath.Base(c.StateFile), filepath.Ext(c.StateFile))
	return filepath.Join(filepath.Dir(c.StateFile), "logs", base)
}

func (c Config) validateLogging() error {
	if c.LogDirectory != "" && (!filepath.IsAbs(c.LogDirectory) || filepath.Clean(c.LogDirectory) != c.LogDirectory || forbiddenRoot(c.LogDirectory)) {
		return errors.New("log_directory 必须为专用的规范绝对目录，不能使用系统或根目录")
	}
	if c.LogKeepDays < 1 || c.LogKeepDays > 3650 {
		return errors.New("log_keep_days 范围1..3650；默认保留30个日历日")
	}
	if c.LogMaxTotalBytes < MiB || c.LogMaxTotalBytes > 1<<40 {
		return errors.New("log_max_total_bytes 范围1MiB..1TiB；默认1000000000字节")
	}
	if c.LogTimezone == "" {
		return errors.New("log_timezone 不能为空；使用 Local、UTC 或 Asia/Shanghai 等时区")
	}
	if _, err := time.LoadLocation(c.LogTimezone); err != nil {
		return errors.New("log_timezone 无效")
	}
	return nil
}
