//go:build linux && (amd64 || arm64)

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type CapabilityCheck struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	NextStep string `json:"next_step"`
}

type Preflight struct {
	ProtectionReady bool              `json:"protection_inputs_ready"`
	Checks          []CapabilityCheck `json:"checks"`
	Scope           string            `json:"scope"`
}

// Doctor-only probes. An unavailable host interface is not proof of a kernel
// option: procfs can be hidden, filtered or namespaced by the container.
func preflight(c Config, m Metrics, readerErr error, read func(string) ([]byte, error)) Preflight {
	p := Preflight{Checks: []CapabilityCheck{}, Scope: "配置保护输入检查；不等于允许回收、目录完整覆盖或业务安全验收。"}
	add := func(code, message, next string) { p.Checks = append(p.Checks, CapabilityCheck{code, message, next}) }
	if readerErr != nil {
		code := "cgroup_resolution_failed"
		if errors.Is(readerErr, os.ErrPermission) {
			code = "cgroup_permission_denied"
		}
		if errors.Is(readerErr, os.ErrNotExist) {
			code = "cgroup_path_missing"
		}
		add(code, "无法定位或读取真实cgroup，不能开始主动回收。", "检查容器cgroup挂载、路径与当前UID；不要用普通目录模拟生产cgroup。")
		return p
	}
	guard := assessProtection(c, m)
	if guard.Experimental {
		add("experimental_limited_output", "显式选择受限备份输出策略；IO字节速率不等价于PSI等待压力，真实业务效果尚待验收。", "只对低复用备份产物试用；先匹配对照，完整备份/校验/恢复及尾延迟验收前不推广到PGDATA。")
	}
	p.ProtectionReady = guard.Ready
	if m.Version == 1 {
		add("cgroup_v1_capability_limits", "当前为cgroup v1，不能假定v2 IO PSI、IO速率或OOM计数接口存在。", "按实际可见字段评审配置；本命令不会关闭保护或改cgroup版本。")
	}
	if m.IOPSI == nil && m.Version == 2 {
		_, err := read(filepath.Join(m.Cgroup, "io.pressure"))
		switch {
		case errors.Is(err, os.ErrPermission):
			add("io_psi_permission_denied", "目标cgroup的IO PSI接口没有读取权限。", "检查挂载、UID与访问策略，避免直接授予容器特权。")
		case err == nil:
			add("io_psi_invalid", "IO PSI接口存在，但没有有效的some avg10百分比。", "检查原始接口内容与采集失败；未知值不作为零压力。")
		case errors.Is(err, os.ErrNotExist):
			_, hostErr := read("/proc/pressure/io")
			if hostErr == nil {
				add("cgroup_io_psi_missing", "宿主可见IO PSI接口存在，但目标cgroup接口缺失。", "检查目标cgroup、挂载可见性与内核cgroup PSI配置。")
			} else if errors.Is(hostErr, os.ErrPermission) {
				add("host_psi_visibility_denied", "目标接口缺失，且不能读取宿主可见PSI接口。", "从宿主只读复核；不能据此断定内核已关闭PSI。")
			} else {
				cmdline, cmdErr := read("/proc/cmdline")
				disabled := false
				if cmdErr == nil {
					for _, arg := range strings.Fields(string(cmdline)) {
						if arg == "psi=0" || arg == "psi=off" {
							disabled = true
						}
					}
				}
				if disabled {
					add("psi_disabled_boot_argument", "当前可见启动参数明确关闭PSI，目标和宿主可见接口均缺失。", "由宿主维护流程评审启动参数与受控重启；CCG不会自动执行。")
				} else {
					add("psi_host_interface_unavailable", "目标和宿主可见PSI接口不可用，尚不能确定是内核配置、默认关闭还是挂载隐藏。", "在宿主核对CONFIG_PSI、CONFIG_PSI_DEFAULT_DISABLED及启动参数；保留严格保护。")
				}
			}
		default:
			add("io_psi_read_failed", "读取IO PSI接口失败。", "核对接口及临时IO错误；采集恢复前不将未知值当零。")
		}
	}
	for _, name := range guard.Missing {
		next := "先修复采集和所需接口；该检查不会自动禁用任何保护。"
		if name == "self_cpu_percent" || name == "io_bytes_per_second" || name == "file_refaults_per_second" {
			next = "速率需要两个连续有效样本；用doctor --require-ready进行双样本部署预检，仍缺失则检查接口。"
		}
		add("required_input_missing:"+name, "启用的保护输入不可用："+name, next)
	}
	if len(guard.Disabled) > 0 {
		add("optional_guards_disabled", "部分可选保护未配置，具体见protection.disabled_guards。", "根据业务与节点监控能力评审阈值；输入就绪不代表所有可选保护已启用。")
	}
	return p
}
