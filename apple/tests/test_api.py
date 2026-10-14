# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json, threading, tempfile, unittest, urllib.request, urllib.error
from server.app import make_server
from server.core import Store
class APITests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.store=Store(self.temp.name+'/db');self.store.tenant('team',8)
        self.tokens={'a'*32:{'role':'admin'},'t'*32:{'role':'tenant','tenant':'team'},'n'*32:{'role':'agent','node':'mini'}}
        self.server=make_server(self.store,self.tokens,port=0);self.thread=threading.Thread(target=self.server.serve_forever,daemon=True);self.thread.start();self.base='http://127.0.0.1:'+str(self.server.server_port)
    def tearDown(self):self.server.shutdown();self.server.server_close();self.thread.join();self.store.db.close();self.temp.cleanup()
    def req(self,path,token='a'*32,body=None):
        r=urllib.request.Request(self.base+path,headers={'Authorization':'Bearer '+token},data=json.dumps(body).encode() if body is not None else None)
        try:
            with urllib.request.urlopen(r) as res:return res.status,json.load(res)
        except urllib.error.HTTPError as e:return e.code,json.load(e)
    def test_auth(self):self.assertEqual(self.req('/api/state','bad')[0],401)
    def test_role_enforcement(self):
        self.assertEqual(self.req('/api/tenants','t'*32,dict(id='hack',quota=4))[0],403)
        self.assertEqual(self.req('/api/state','n'*32)[0],403)
        self.assertEqual(self.req('/api/agent/claim',body={})[0],403)
    def test_tenant_cannot_spoof_owner(self):
        status,j=self.req('/api/jobs','t'*32,dict(runtime='demo',memoryGiB=4,tenant='default'));self.assertEqual(status,201)
        self.assertEqual(self.store.snapshot()['jobs'][0]['tenant'],'team')
    def test_end_to_end(self):
        _,j=self.req('/api/jobs',body=dict(runtime='demo',memoryGiB=4))
        self.assertEqual(self.req('/api/agent/heartbeat','n'*32,dict(budgetGiB=8,runtimes=['demo']))[0],200)
        _,c=self.req('/api/agent/claim','n'*32,{});c=c['job']
        self.assertEqual(c['id'],j['id']);self.assertEqual(self.req('/api/agent/log','n'*32,dict(id=c['id'],lease=c['lease'],text='native worker completed'))[0],200)
        self.assertEqual(self.req('/api/agent/complete','n'*32,dict(id=c['id'],lease=c['lease'],success=True,result={'exitCode':0}))[0],200)
        self.assertEqual(self.req('/api/state')[1]['jobs'][0]['state'],'succeeded');self.assertEqual(len(self.req('/api/logs/'+c['id'])[1]),1)
    def test_agent_log_batch_http(self):
        job=self.store.submit('team',dict(runtime='demo',memoryGiB=4))
        self.store.register('mini',dict(budgetGiB=8,runtimes=['demo']))
        claim=self.store.claim('mini')
        payload=dict(id=job,lease=claim['lease'],lines=['first','second','third'])
        self.assertEqual(self.req('/api/agent/logs','n'*32,payload)[0],200)
        self.assertEqual([r['text'] for r in self.req('/api/logs/'+job,'t'*32)[1]],payload['lines'])
        self.assertEqual(self.req('/api/agent/logs','t'*32,payload)[0],403)
        payload['lease']='stale'
        self.assertEqual(self.req('/api/agent/logs','n'*32,payload)[0],409)
    def test_agent_log_batch_bad_fields(self):
        job=self.store.submit('default',dict(runtime='demo',memoryGiB=4))
        self.store.register('mini',dict(budgetGiB=8,runtimes=['demo']))
        claim=self.store.claim('mini')
        for lines in [[],None,'text',[1],['x']*101]:
            self.assertEqual(self.req('/api/agent/logs','n'*32,dict(id=job,lease=claim['lease'],lines=lines))[0],400)
    def test_invalid_json_fields(self):self.assertEqual(self.req('/api/jobs',body={})[0],400)
    def test_no_path_traversal(self):self.assertEqual(self.req('/api/not-found')[0],404)
    def test_static_csp_and_no_secret(self):
        with urllib.request.urlopen(self.base) as r:
            self.assertIn("frame-ancestors 'none'",r.headers['Content-Security-Policy']);self.assertNotIn('a'*32,r.read().decode())
    def test_tenant_cannot_read_other_logs(self):
        j=self.store.submit('default',dict(runtime='demo',memoryGiB=4));self.assertEqual(self.req('/api/logs/'+j,'t'*32)[0],404)
if __name__=='__main__':unittest.main()
