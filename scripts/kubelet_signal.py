#!/usr/bin/env python3
"""Optional read-only collector. Core CCG has no Python/Kubernetes dependency.

Uses existing kubectl credentials; does not create RBAC or change cluster state.
Failure leaves the last signal untouched so CCG expires it, never renews it.
"""
import argparse
import datetime as dt
from decimal import Decimal, ROUND_CEILING
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

ID = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,127}\Z")

def quantity(value, capacity=None):
    if not isinstance(value, str):
        raise ValueError("quantity must be text")
    if value.endswith('%'):
        n = Decimal(value[:-1])
        if capacity is None or not n.is_finite() or not 0 <= n <= 100:
            raise ValueError("invalid percentage/capacity")
        result = n * capacity / 100
    else:
        match = re.fullmatch(r'([0-9]+(?:\.[0-9]+)?)([KMGTPE]i|[kKMGTPE]|m|u|n|[eE][+-]?[0-9]+)?', value)
        if not match:
            raise ValueError("unsupported quantity")
        number, suffix = match.groups(); suffix = suffix or ''
        factors = {'': Decimal(1), 'm': Decimal('.001'), 'u': Decimal('.000001'), 'n': Decimal('.000000001')}
        if suffix.endswith('i'):
            factor = Decimal(1024) ** ('KMGTPE'.index(suffix[0]) + 1)
        elif suffix in ('k', 'K', 'M', 'G', 'T', 'P', 'E'):
            factor = Decimal(1000) ** ('KMGTPE'.index(suffix.upper()) + 1)
        elif suffix.startswith(('e', 'E')):
            exponent = int(suffix[1:])
            if abs(exponent) > 30:
                raise ValueError('quantity exponent out of range')
            factor = Decimal(10) ** exponent
        else:
            factor = factors[suffix]
        result = Decimal(number) * factor
    if not result.is_finite() or not 0 <= result <= 2**63-1:
        raise ValueError('quantity overflow/negative')
    # Round upward so stop thresholds are never understated by a fraction byte.
    return int(result.to_integral_value(rounding=ROUND_CEILING))

def duration_seconds(value):
    if not isinstance(value, str):
        raise ValueError('duration must be text')
    parts = re.findall(r'([0-9]+(?:\.[0-9]+)?)(ns|us|µs|ms|s|m|h)', value)
    if not parts or ''.join(a+b for a,b in parts) != value:
        if value == '0':
            return 0
        raise ValueError('invalid grace duration')
    units = {'ns': Decimal('1e-9'), 'us': Decimal('1e-6'), 'µs': Decimal('1e-6'), 'ms': Decimal('.001'), 's': Decimal(1), 'm': Decimal(60), 'h': Decimal(3600)}
    value = sum(Decimal(a)*units[b] for a,b in parts)
    if value > 2**63-1:
        raise ValueError('grace overflow')
    return int(value.to_integral_value(rounding=ROUND_CEILING))

def normalize(summary, configz, node, source_id, now, max_age):
    if not ID.fullmatch(source_id):
        raise ValueError('invalid source id')
    uid = node['metadata']['uid']; name = node['metadata']['name']
    if not ID.fullmatch(uid) or summary['node']['nodeName'] != name:
        raise ValueError('node identity mismatch')
    memory = summary['node']['memory']
    stamp = dt.datetime.fromisoformat(memory['time'].replace('Z', '+00:00'))
    if stamp.tzinfo is None or not -1 <= (now-stamp).total_seconds() <= max_age:
        raise ValueError('Summary memory sample stale/future')
    available = memory['availableBytes']
    if type(available) is not int or not 0 <= available <= 2**63-1:
        raise ValueError('invalid Summary memory.available')
    config = configz['kubeletconfig']
    # These must be supplied by live configz, not guessed from version defaults.
    hard = config['evictionHard']
    # In the KubeletConfiguration API these maps are omitempty and soft
    # eviction has no default. Absence in live configz means no soft policy.
    soft = config.get('evictionSoft', {})
    grace = config.get('evictionSoftGracePeriod', {})
    if not all(isinstance(x, dict) for x in (hard, soft, grace)):
        raise ValueError('effective eviction maps unavailable')
    capacity = quantity(node['status']['capacity']['memory'])
    hard_bytes = quantity(hard['memory.available'], capacity) if 'memory.available' in hard else 0
    soft_bytes = quantity(soft['memory.available'], capacity) if 'memory.available' in soft else 0
    soft_grace = duration_seconds(grace['memory.available']) if 'memory.available' in soft else 0
    conditions = [c for c in node['status']['conditions'] if c['type'] == 'MemoryPressure']
    if len(conditions) != 1 or conditions[0]['status'] not in ('True', 'False'):
        raise ValueError('MemoryPressure is unknown')
    return dict(schema_version=1, source_id=source_id, node_id=uid, time=memory['time'],
                memory_available_bytes=available, hard_available_bytes=hard_bytes,
                soft_available_bytes=soft_bytes, soft_grace_seconds=soft_grace,
                memory_pressure=conditions[0]['status']=='True')

def get_json(arguments):
    # Fixed executable, argument array, no shell/custom executable hooks.
    result = subprocess.run(['kubectl', '--request-timeout=8s', *arguments],
                            capture_output=True, timeout=10, check=True)
    if len(result.stdout)>4*1024*1024:
        raise ValueError('kubectl response too large')
    return json.loads(result.stdout)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--node', required=True, help='Kubernetes node name')
    parser.add_argument('--source-id', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--interval', type=float, default=2)
    parser.add_argument('--max-age', type=int, default=30)
    parser.add_argument('--once', action='store_true')
    args = parser.parse_args()
    if not re.fullmatch(r'[a-z0-9][a-z0-9.-]{0,252}', args.node) or not ID.fullmatch(args.source_id) or not 1 <= args.interval <= 60 or not 1 <= args.max_age <= 30 or not args.output.is_absolute():
        parser.error('invalid node/source id/interval/age/output')
    while True:
        try:
            node = get_json(['get','node',args.node,'-o','json'])
            summary = get_json(['get','--raw',f'/api/v1/nodes/{args.node}/proxy/stats/summary'])
            configz = get_json(['get','--raw',f'/api/v1/nodes/{args.node}/proxy/configz'])
            result = normalize(summary,configz,node,args.source_id,dt.datetime.now(dt.timezone.utc),args.max_age)
            # Directory must already be provisioned by the operator.
            with tempfile.NamedTemporaryFile(mode='w',encoding='utf-8',dir=args.output.parent,delete=False) as stream:
                temporary=Path(stream.name)
                try:
                    json.dump(result,stream);stream.write('\n');stream.close()
                    os.chmod(temporary,0o644);os.replace(temporary,args.output)
                finally:
                    temporary.unlink(missing_ok=True)
            print(json.dumps({'event':'node_signal_written','sample_time':result['time'],'node_id':result['node_id']}),flush=True)
        except Exception as error:
            # Do not print kubectl stderr: it may contain configuration details.
            print(json.dumps({'event':'node_signal_failed','error_type':type(error).__name__}),file=sys.stderr,flush=True)
            if args.once:
                return 1
        if args.once:
            return 0
        time.sleep(args.interval)

if __name__=='__main__':
    raise SystemExit(main())
