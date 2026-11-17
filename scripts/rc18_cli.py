#!/usr/bin/env python3
"""Task wrapper CLI contract checks, using only private temporary processes."""
import argparse, datetime, json, os, pathlib, signal, subprocess, sys, tempfile, time


def main():
    ap=argparse.ArgumentParser();ap.add_argument('binary');ap.add_argument('--output',required=True);a=ap.parse_args()
    binary=str(pathlib.Path(a.binary).resolve());checks=[]
    with tempfile.TemporaryDirectory(prefix='ccg-rc18-cli-') as tmp:
        root=pathlib.Path(tmp);tasks=root/'tasks';tasks.mkdir(mode=0o700)
        prefix=[binary,'task-run','--directory',str(tasks),'--source-id','cli','--instance-id','cli','--stage','sync','--root',str(root),'--']
        result=subprocess.run(prefix+[sys.executable,'-c','import sys;print(sys.argv[1]);sys.exit(7)','a b;$x'],capture_output=True,text=True,timeout=10)
        assert result.returncode==7 and result.stdout=='a b;$x\n' and not list(tasks.iterdir())
        checks.append('literal_arguments_stdout_and_exit7_preserved')
        p=subprocess.Popen(prefix+[sys.executable,'-u','-c','import signal,sys,time;signal.signal(signal.SIGTERM,lambda *_:sys.exit(23));print("ready");time.sleep(30)'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        try:
            assert p.stdout.readline().strip()=='ready'
            marks=list(tasks.glob('*.json'));assert len(marks)==1
            record=json.loads(marks[0].read_text());assert record['stage']=='sync' and 'command' not in record
            p.send_signal(signal.SIGTERM);stdout,stderr=p.communicate(timeout=10)
            assert p.returncode==23 and not list(tasks.iterdir()),(p.returncode,stderr)
        finally:
            if p.poll() is None:p.kill();p.wait(5)
        checks.append('SIGTERM_forwarded_to_child_and_exit23_preserved')
        p=subprocess.Popen(prefix+[sys.executable,'-u','-c','import time;print("ready");time.sleep(3.5)'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
        try:
            assert p.stdout.readline().strip()=='ready'
            tasks.rename(root/'old-tasks');tasks.mkdir(mode=0o700)
            stdout,stderr=p.communicate(timeout=10)
            assert p.returncode==0 and 'heartbeat failed' in stderr
            assert list((root/'old-tasks').glob('*.json')) and not list(tasks.iterdir())
        finally:
            if p.poll() is None:p.kill();p.wait(5)
        checks.append('heartbeat_failure_preserves_business_exit_and_stale_evidence')
        # An explicitly synthetic authenticated-API fixture; no cluster access.
        # Exercise real command wiring and ensure failed reads NEVER renew data.
        fixture=root/'kubectl';version_file=root/'kubelet-version';version_file.write_text('v1.31.0')
        fixture.write_text('''#!/usr/bin/env python3
import datetime,json,pathlib,sys
assert '--insecure-skip-tls-verify=false' in sys.argv
now=datetime.datetime.now(datetime.timezone.utc).isoformat()
version=(pathlib.Path(__file__).parent/'kubelet-version').read_text()
if '/stats/summary' in sys.argv[-1]:
 value={'node':{'nodeName':'node-1','memory':{'time':now,'availableBytes':4000000000,'workingSetBytes':4000000000}}}
elif '/configz' in sys.argv[-1]:
 value={'kubeletconfig':{'evictionHard':{'memory.available':'100Mi'},'enforceNodeAllocatable':[],'cgroupsPerQOS':True}}
else:
 value={'metadata':{'uid':'node-fixture','name':'node-1'},'status':{'nodeInfo':{'bootID':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'operatingSystem':'linux','kubeletVersion':version},'conditions':[{'type':'MemoryPressure','status':'False','lastHeartbeatTime':now}]}}
print(json.dumps(value))
''');fixture.chmod(0o700)
        envelope=root/'node-v2.json';legacy=root/'node-v1.json'
        env=dict(os.environ,PATH=str(root)+os.pathsep+os.environ['PATH'])
        args=[binary,'node-collect','--once','--node','node-1','--source-id','synthetic-api-fixture','--output',str(envelope),'--legacy-output',str(legacy)]
        result=subprocess.run(args,env=env,capture_output=True,text=True,timeout=20)
        assert result.returncode==0,result.stderr
        node=json.loads(envelope.read_text());old=json.loads(legacy.read_text())
        assert node['schema_version']==2 and old['schema_version']==1 and node['time']==old['time']
        assert node['disk_await_ms'] is None and node['business_samples'] is None
        checks.append('collector_command_preserves_synthetic_source_time_and_unknowns')
        original=envelope.read_bytes();stamp=envelope.stat().st_mtime_ns;version_file.write_text('v1.99.0')
        result=subprocess.run(args,env=env,capture_output=True,text=True,timeout=20)
        assert result.returncode!=0 and envelope.read_bytes()==original and envelope.stat().st_mtime_ns==stamp
        checks.append('collector_failure_does_not_renew_previous_signal')
    report={'success':True,'checks':checks}
    pathlib.Path(a.output).write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
