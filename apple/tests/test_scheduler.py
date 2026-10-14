# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import concurrent.futures, tempfile, unittest, math
from server.core import Store, Problem

class SchedulerTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(); self.now=100.; self.store=Store(self.temp.name+'/db',lambda:self.now)
        self.node('mini'); self.store.tenant('team',8)
    def tearDown(self): self.store.db.close(); self.temp.cleanup()
    def node(self,name,budget=8,slots=1,**kw): self.store.register(name,dict(budgetGiB=budget,slots=slots,runtimes=['demo','mlx'],**kw))
    def job(self,tenant='default',**kw):
        self.now+=0.001
        return self.store.submit(tenant,dict(runtime='demo',memoryGiB=4,**kw))
    def test_memory_fit(self):
        j=self.store.submit('default',dict(runtime='demo',memoryGiB=9)); self.assertIsNone(self.store.claim('mini')); self.assertIn('budget',self.store.snapshot()['jobs'][0]['reason'])
    def test_priority(self):
        self.job(priority=1); high=self.job(priority=99); self.assertEqual(self.store.claim('mini')['id'],high)
    def test_tenant_quota(self):
        self.node('mini',16,4); self.job('team',mode='shared'); self.job('team',mode='shared'); self.job('team',mode='shared')
        self.assertIsNotNone(self.store.claim('mini'));self.assertIsNotNone(self.store.claim('mini'));self.assertIsNone(self.store.claim('mini'))
    def test_exclusive_blocks_shared(self):
        self.node('mini',16,4); self.job();self.job(mode='shared');self.store.claim('mini');self.assertIsNone(self.store.claim('mini'))
    def test_shared_blocks_exclusive(self):
        self.node('mini',16,4);self.job(mode='shared',priority=100);self.job();self.store.claim('mini');self.assertIsNone(self.store.claim('mini'))
    def test_stale_worker_fenced(self):
        j=self.job(retries=1);c=self.store.claim('mini');self.now+=21
        with self.assertRaises(Problem):self.store.complete('mini',j,c['lease'],True,{})
        c2=self.store.claim('mini');self.assertNotEqual(c['lease'],c2['lease'])
    def test_retry_exhausted(self):
        j=self.job(retries=1);c=self.store.claim('mini');self.store.complete('mini',j,c['lease'],False,{})
        c=self.store.claim('mini');self.store.complete('mini',j,c['lease'],False,{})
        self.assertEqual(self.store.snapshot()['jobs'][0]['state'],'failed')
    def test_running_cancel_holds_until_ack(self):
        j=self.job();self.job();c=self.store.claim('mini');self.store.cancel(j)
        self.assertIsNone(self.store.claim('mini'));self.assertTrue(self.store.renew('mini',j,c['lease'])['cancel'])
        self.store.complete('mini',j,c['lease'],True,{});self.assertIsNotNone(self.store.claim('mini'))
    def test_queued_cancel(self):
        j=self.job();self.store.cancel(j);self.assertIsNone(self.store.claim('mini'))
    def test_cancelled_expiry_never_requeues(self):
        j=self.job(retries=10);self.store.claim('mini');self.store.cancel(j);self.now+=21
        self.assertEqual(self.store.snapshot()['jobs'][0]['state'],'cancelled')
    def test_renew_extends(self):
        j=self.job();c=self.store.claim('mini');self.now+=10;self.store.renew('mini',j,c['lease']);self.now+=11
        self.assertEqual(self.store.snapshot()['jobs'][0]['state'],'running')
    def test_concurrent_claim_no_double_allocate(self):
        self.node('other');j=self.job()
        with concurrent.futures.ThreadPoolExecutor() as pool: results=list(pool.map(self.store.claim,['mini','other']))
        self.assertEqual(sum(x is not None for x in results),1)
    def test_drain(self):
        self.job();self.store.drain('mini',True);self.assertIsNone(self.store.claim('mini'));self.store.drain('mini',False);self.assertIsNotNone(self.store.claim('mini'))
    def test_stale_node(self):
        self.job();self.now+=31;self.assertIsNone(self.store.claim('mini'))
    def test_thermal_gate(self):
        self.node('mini',thermal='serious');self.job();self.assertIsNone(self.store.claim('mini'))
    def test_pressure_gate(self):
        self.node('mini',pressure='warning');self.job();self.assertIsNone(self.store.claim('mini'))
    def test_runtime_selector(self):
        self.job(runtime_unused='ignored')
        self.store.register('mini',dict(budgetGiB=8,runtimes=['mlx']));self.assertIsNone(self.store.claim('mini'))
    def test_node_selector(self):
        self.job(node='another');self.assertIsNone(self.store.claim('mini'))
    def test_tenant_visibility(self):
        a=self.job('team');self.job();s=self.store.snapshot('team');self.assertEqual([j['id'] for j in s['jobs']],[a]);self.assertEqual(s['nodes'],[])
        with self.assertRaises(Problem):self.store.cancel(a,'default')
    def test_lease_hidden(self):
        self.job();self.store.claim('mini');self.assertNotIn('lease',self.store.snapshot()['jobs'][0])
    def test_invalid_inputs(self):
        for m in [0,-1,True,float('nan'),float('inf'),'4']:
            with self.assertRaises(Problem):self.store.submit('default',dict(runtime='demo',memoryGiB=m))
        for p in [-1,101,True,'50']:
            with self.assertRaises(Problem):self.job(priority=p)
    def test_log_fencing(self):
        j=self.job();c=self.store.claim('mini');self.store.log('mini',j,c['lease'],'hello')
        with self.assertRaises(Problem):self.store.log('other',j,c['lease'],'bad')
    def test_restart_persists_reservation(self):
        self.job();self.store.claim('mini');other=Store(self.temp.name+'/db',lambda:self.now)
        self.assertEqual(other.snapshot()['jobs'][0]['state'],'running');self.assertIsNone(other.claim('mini'));other.db.close()
    def test_node_identity_fields_rejected(self):
        with self.assertRaises(Problem):self.node('mini',id='spoof')
    def test_catalog_marking(self):
        self.store.model('private',dict(memoryGiB=4,tenants=['team']))
        self.assertEqual(len(self.store.snapshot('default')['models']),0)
        self.assertEqual(len(self.store.snapshot('team')['models']),1)
    def test_metering(self):
        j=self.job();c=self.store.claim('mini');self.now+=5;self.store.complete('mini',j,c['lease'],True,{})
        u=self.store.snapshot()['usage'][0];self.assertEqual(u['workerSeconds'],5);self.assertEqual(u['reservedGiBSeconds'],20)
    def test_weighted_queue_order(self):
        self.node('mini',32,8);self.store.tenant('team',16,1);self.store.tenant('default',16,1)
        self.job('team',priority=100,mode='shared');self.store.claim('mini')
        self.job('team',mode='shared');fair=self.job('default',mode='shared');self.assertEqual(self.store.claim('mini')['id'],fair)

if __name__=='__main__':unittest.main()
