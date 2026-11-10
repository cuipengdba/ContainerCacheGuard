// Exercise the actual embedded page without browser/network dependencies.
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const src = fs.readFileSync(path.join(__dirname, '..', 'web_linux.go'), 'utf8');
const page = src.slice(src.indexOf('const webPage = `') + 'const webPage = `'.length);
const script = page.split('<script>')[1].split('</script>')[0]
  .replace(/update\(\);history\(\);setInterval[^\n]*/, '');
const elements = new Map(), moves = [], lines = [];
const canvas = {
  scale() {}, clearRect() {}, beginPath() {}, stroke() {}, fillText() {},
  moveTo(x, y) { moves.push([this.strokeStyle, x, y]); },
  lineTo(x, y) { lines.push([this.strokeStyle, x, y]); },
};
const context = vm.createContext({
  document: {getElementById(id) {
    if (!elements.has(id)) elements.set(id, {textContent: '', innerHTML: '', className: '', clientWidth: 900, getContext: () => canvas});
    return elements.get(id);
  }}, devicePixelRatio: 1, console,
});
vm.runInContext(script, context);
const now = '2026-11-10T00:00:00Z';
context.sample = {
  time: now, mode: 'paths', action: 'enforce', decision: {state: 'CONTROL', reason: 'cache_budget_exceeded'},
  counters: {pending_hint_paths: 2}, roots: ['/backup'], diagnostics: {valid: true, other_charged_bytes: 100, notes: [{message: '普通文件页以外的内存不由文件接口清理'}]},
  metrics: {version: 2, file_bytes: 200, shmem_bytes: 100, current_bytes: 500, file_refaults_per_second: 0, file_refault_rate_valid: false},
};
context.now = now;
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('detail').textContent, /refault：未知/);
assert.match(elements.get('explain').textContent, /等待 2 个/);
context.sample.metrics.file_refault_rate_valid = true;
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('detail').textContent, /refault：0\.0/);
context.sample.recovery = {pending: true, healthy_seconds: 1.5, required_seconds: 10};
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('detail').textContent, /等待连续有效采样 1\.5 \/ 10 秒/);
context.sample.metrics.error = 'cgroup read failed';
vm.runInContext('render(sample, now)', context);
assert.equal((elements.get('cards').innerHTML.match(/未知/g) || []).length, 10);
assert.match(elements.get('detail').textContent, /IO：未知/);
assert.match(elements.get('detail').textContent, /OOM kill：未知/);
delete context.sample.metrics.error;
context.sample.metrics.available_metrics = {file_bytes: true, current_bytes: true, shmem_bytes: false};
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('cards').innerHTML, /共享内存 shmem<\/div><div[^>]*>未知/);
assert.match(elements.get('cards').innerHTML, /普通文件缓存<\/div><div[^>]*>未知/);
context.sample.metrics.current_bytes = 500;
context.sample.metrics.available_metrics.current_bytes = false;
context.sample.metrics.io_psi_avg10 = 3;
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('detail').textContent, /本容器用量：未知/);
assert.match(elements.get('detail').textContent, /IO PSI：未知/);
context.sample.metrics.available_metrics.current_bytes = true;
context.sample.metrics.available_metrics.io_psi_avg10 = true;
context.sample.metrics.time = '2026-11-09T23:59:50Z';
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('state').textContent, /已过期/);
assert.match(elements.get('detail').textContent, /IO PSI：未知/);
assert.equal((elements.get('cards').innerHTML.match(/未知/g) || []).length, 10);
context.sample.metrics.time = '2026-11-10T00:00:02Z';
vm.runInContext('render(sample, now)', context);
assert.match(elements.get('state').textContent, /已过期/);
context.historyData = [0, 1, 2, 4, 5, 30, 32].map(seconds => ({
  time: new Date(Date.parse(now) + seconds * 1000).toISOString(), valid: seconds !== 2, sample_interval_ms: 1000,
  file_bytes: 200, active_file_bytes: 100, anon_bytes: 100, shmem_bytes: 50, current_bytes: 500,
}));
vm.runInContext('graph(historyData)', context);
// Three contiguous segments per series; invalid samples and the long gap are
// not drawn as healthy zeroes or bridged with invented continuous lines.
assert.equal(lines.filter(p => p[0] === '#7bb8ff').length, 3);
assert.equal(moves.filter(p => p[0] === '#7bb8ff').length, 3);
const series = lines.filter(p => p[0] === '#7bb8ff');
assert.ok(series[2][1] - series[1][1] > 5 * (series[1][1] - series[0][1]), 'horizontal spacing must follow elapsed time');
moves.length = 0; lines.length = 0;
context.historyData = [0, 1, 2].map(seconds => ({
  time: new Date(Date.parse(now) + seconds * 1000).toISOString(), valid: true, sample_interval_ms: 1000,
  unavailable: seconds === 1 ? ['file_bytes'] : [],
  file_bytes: 200, active_file_bytes: 100, anon_bytes: 100, shmem_bytes: 50, current_bytes: 500,
}));
vm.runInContext('graph(historyData)', context);
assert.equal(lines.filter(p => p[0] === '#7bb8ff').length, 0);
assert.equal(lines.filter(p => p[0] === '#eb91be').length, 2);
console.log(JSON.stringify({success: true, checks: ['unknown_refault', 'known_zero_refault', 'missing_metrics_cards', 'pending_hint_explanation', 'healthy_recovery_explanation', 'invalid_history_gap', 'elapsed_time_axis', 'missing_memory_field', 'stale_metric_timestamp', 'future_metric_timestamp', 'per_field_history_gap', 'unavailable_detail_values']}));
