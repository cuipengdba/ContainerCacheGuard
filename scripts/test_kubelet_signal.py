import copy
import datetime as dt
import unittest
from kubelet_signal import normalize, quantity, duration_seconds

class SignalTests(unittest.TestCase):
    def setUp(self):
        self.now=dt.datetime(2026,11,11,tzinfo=dt.timezone.utc)
        self.summary={'node':{'nodeName':'node1','memory':{'time':self.now.isoformat(),'availableBytes':100000}}}
        self.config={'kubeletconfig':{'evictionHard':{'memory.available':'100Mi'},'evictionSoft':{'memory.available':'5%'},'evictionSoftGracePeriod':{'memory.available':'1m30s'}}}
        self.node={'metadata':{'name':'node1','uid':'node-uid'},'status':{'capacity':{'memory':'1Gi'},'conditions':[{'type':'MemoryPressure','status':'False'}]}}
    def sample(self):
        return normalize(self.summary,self.config,self.node,'collector-1',self.now,30)
    def test_actual_summary_and_effective_thresholds(self):
        result=self.sample();self.assertEqual(result['memory_available_bytes'],100000)
        self.assertEqual(result['hard_available_bytes'],100*1024**2)
        self.assertEqual(result['soft_available_bytes'],53687092)
        self.assertEqual(result['soft_grace_seconds'],90)
    def test_unknown_stale_identity_and_missing_effective_config_fail(self):
        for key in ['unknown','stale','identity','config','grace','negative','boolean']:
            old=copy.deepcopy((self.summary,self.config,self.node))
            if key=='unknown':self.node['status']['conditions'][0]['status']='Unknown'
            if key=='stale':self.summary['node']['memory']['time']='2026-11-10T00:00:00Z'
            if key=='identity':self.summary['node']['nodeName']='other'
            if key=='config':del self.config['kubeletconfig']['evictionHard']
            if key=='grace':del self.config['kubeletconfig']['evictionSoftGracePeriod']['memory.available']
            if key=='negative':self.summary['node']['memory']['availableBytes']=-1
            if key=='boolean':self.summary['node']['memory']['availableBytes']=True
            with self.assertRaises((ValueError,KeyError)):self.sample()
            self.summary,self.config,self.node=old
    def test_quantities_and_grace_fail_closed(self):
        self.assertEqual(quantity('1.5Gi'),1610612736);self.assertEqual(quantity('1e3'),1000)
        self.assertEqual(duration_seconds('1m0.1s'),61)
        for value in ['NaN%','101%','-1Mi','9e999','1Zi','Infinity','1Mi junk']:
            with self.assertRaises((ValueError,KeyError)):quantity(value,100)

if __name__=='__main__':unittest.main()
