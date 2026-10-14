# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import json,threading,tempfile,unittest
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from server.inference import Gateway
from server.core import Store,Problem
class Worker(BaseHTTPRequestHandler):
    def log_message(self,*a):pass
    def do_POST(self):
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])));self.server.seen=(dict(self.headers),body)
        if body.get('stream'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            for part in ['Hello',' from',' stream']:
                self.wfile.write(('data: '+json.dumps({'choices':[{'delta':{'content':part}}]})+'\n\n').encode());self.wfile.flush()
            self.wfile.write(b'data: [DONE]\n\n');return
        result=json.dumps({'choices':[{'message':{'role':'assistant','content':'Hello from mock worker'}}],'usage':{'completion_tokens':2}}).encode()
        self.send_response(200);self.send_header('Content-Length',str(len(result)));self.end_headers();self.wfile.write(result)
class InferenceTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.store=Store(self.temp.name+'/db');self.worker=ThreadingHTTPServer(('127.0.0.1',0),Worker);self.thread=threading.Thread(target=self.worker.serve_forever,daemon=True);self.thread.start()
        self.origin='http://127.0.0.1:'+str(self.worker.server_port);self.g=Gateway([self.origin]);self.store.model('model',dict(memoryGiB=4,endpoints=[self.origin]));self.body={'model':'model','messages':[{'role':'user','content':'hi'}]}
    def tearDown(self):self.worker.shutdown();self.worker.server_close();self.thread.join();self.store.db.close();self.temp.cleanup()
    def test_routes_real_http(self):
        r=self.g.chat(self.store,'default',self.body);self.assertIn('choices',r);self.assertNotIn('Authorization',self.worker.seen[0]);self.assertEqual(self.worker.seen[1]['max_tokens'],256)
    def test_deny_unlisted_origin(self):
        with self.assertRaises(Problem):Gateway().chat(self.store,'default',self.body)
    def test_model_tenant_marking(self):
        self.store.model('model',dict(memoryGiB=4,endpoints=[self.origin],tenants=['other']))
        with self.assertRaises(Problem):self.g.chat(self.store,'default',self.body)
    def test_request_bounds(self):
        for extra in [{'stream':True},{'max_tokens':5000},{'max_tokens':True},{'messages':[{'role':'user','content':'a'*40000}]}]:
            with self.assertRaises(Problem):self.g.chat(self.store,'default',{**self.body,**extra})
    def test_concurrency_gate(self):
        self.g.calls['default']=2
        with self.assertRaises(Problem) as e:self.g.chat(self.store,'default',self.body)
        self.assertEqual(e.exception.status,429)
    def test_counter_released(self):
        self.g.chat(self.store,'default',self.body);self.assertEqual(self.g.calls['default'],0);self.assertEqual(self.g.active[self.origin],0)
    def test_stream_relays_sse_and_releases_slot(self):
        chunks=list(self.g.stream(self.store,'default',{**self.body,'stream':True}))
        text=b''.join(chunks).decode()
        self.assertIn('data: ',text);self.assertIn('[DONE]',text)
        self.assertTrue(self.worker.seen[1]['stream']);self.assertNotIn('Authorization',self.worker.seen[0])
        self.assertEqual(self.g.calls['default'],0);self.assertEqual(self.g.active[self.origin],0)
    def test_stream_releases_slot_when_client_closes_early(self):
        it=self.g.stream(self.store,'default',{**self.body,'stream':True});next(it);it.close()
        self.assertEqual(self.g.calls['default'],0)
    def test_stream_respects_byte_bound(self):
        got=b''.join(self.g.stream(self.store,'default',{**self.body,'stream':True},max_bytes=30))
        self.assertLessEqual(len(got),30)
    def test_stream_gate_and_validation(self):
        self.g.calls['default']=2
        with self.assertRaises(Problem) as e:self.g.stream(self.store,'default',{**self.body,'stream':True})
        self.assertEqual(e.exception.status,429)
        self.g.calls['default']=0
        with self.assertRaises(Problem):self.g.stream(self.store,'default',{**self.body,'stream':True,'max_tokens':99999})
if __name__=='__main__':unittest.main()


class LoopbackReplicaTests(unittest.TestCase):
    def test_loopback_allowed_only_when_enabled(self):
        import tempfile
        from server.inference import Gateway
        from server.core import Store, Problem
        with tempfile.TemporaryDirectory() as d:
            s = Store(d + "/db")
            s.model("m", dict(memoryGiB=1, runtime="llama", endpoints=["http://127.0.0.1:1"]))
            with self.assertRaises(Problem) as e:
                Gateway().chat(s, "default", dict(model="m", messages=[dict(role="user", content="hi")]))
            self.assertEqual(e.exception.message, "no allowlisted inference replica")
            with self.assertRaises(Problem) as e:   # loopback allowed -> reaches the (closed) port, i.e. a replica failure, not an allowlist rejection
                Gateway(allow_loopback=True).chat(s, "default", dict(model="m", messages=[dict(role="user", content="hi")]))
            self.assertEqual(e.exception.message, "inference replica failed")
            s.db.close()
