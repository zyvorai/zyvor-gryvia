#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import argparse, json, os, secrets, ssl
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
from pathlib import Path
from urllib.parse import urlsplit
from server.core import Store, Problem
from server.inference import Gateway

class API(BaseHTTPRequestHandler):
    def log_message(self,*args): pass
    def send(self,status,body,mime='application/json'):
        data=json.dumps(body).encode() if mime=='application/json' else body
        self.send_response(status); self.send_header('Content-Type',mime); self.send_header('Content-Length',str(len(data)))
        self.send_header('Cache-Control','no-store'); self.send_header('X-Content-Type-Options','nosniff')
        self.send_header('Content-Security-Policy',"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
        self.end_headers(); self.wfile.write(data)
    def actor(self):
        raw=self.headers.get('Authorization','')
        token=raw[7:] if raw.startswith('Bearer ') else ''
        for key,actor in self.server.tokens.items():
            if secrets.compare_digest(token,key): return actor
        raise Problem('authentication required',401)
    def body(self):
        size=int(self.headers.get('Content-Length','0'))
        if size<0 or size>65536: raise Problem('request too large',413)
        try: b=json.loads(self.rfile.read(size))
        except (ValueError,UnicodeError): raise Problem('invalid JSON')
        if not isinstance(b,dict): raise Problem('object required')
        return b
    def stream(self,store,tenant,admin,interval=1.0,limit=600):
        """Server-Sent Events: one JSON state snapshot per `interval` seconds, bounded to `limit` frames (clients reconnect)."""
        self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.send_header('Cache-Control','no-store')
        self.send_header('X-Accel-Buffering','no'); self.end_headers()
        import time as _t
        try:
            for i in range(limit):
                snap=store.snapshot(tenant)
                if admin: snap['telemetry']=store.telemetry(60)
                self.wfile.write(b'data: '+json.dumps(snap).encode()+b'\n\n'); self.wfile.flush()
                _t.sleep(interval)
        except (BrokenPipeError,ConnectionResetError,OSError): pass
        self.close_connection=True
    def do_GET(self): self.handle_request(False)
    def do_POST(self): self.handle_request(True)
    def handle_request(self,write):
        try:
            path=urlsplit(self.path).path
            if not write and path=='/healthz': return self.send(200,{'status':'ok'})
            if path == '/v1/chat/completions':
                actor=self.actor()
                if not write or actor['role'] not in ['admin','tenant']: raise Problem('forbidden',403)
                body=self.body()
                if body.get('stream') is True:
                    chunks=self.server.gateway.stream(self.server.store,actor.get('tenant','default'),body)
                    self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.send_header('Cache-Control','no-store'); self.send_header('X-Content-Type-Options','nosniff')
                    self.end_headers()
                    try:
                        for c in chunks: self.wfile.write(c); self.wfile.flush()
                    except (BrokenPipeError,ConnectionResetError,OSError): pass
                    finally: chunks.close()
                    self.close_connection=True
                    return
                return self.send(200,self.server.gateway.chat(self.server.store,actor.get('tenant','default'),body))
            if not path.startswith('/api/') and path!='/metrics':
                if write: raise Problem('not found',404)
                names={'/':'index.html','/app.js':'app.js','/style.css':'style.css','/tokens.css':'tokens.css','/favicon.svg':'favicon.svg','/icon.png':'icon.png'}
                if path not in names: raise Problem('not found',404)
                f=self.server.web/names[path]; mime={'html':'text/html','js':'application/javascript','css':'text/css','svg':'image/svg+xml','png':'image/png'}[f.suffix[1:]]
                return self.send(200,f.read_bytes(),mime)
            actor=self.actor(); role=actor['role']; store=self.server.store
            if not write and path=='/api/state':
                if role=='agent': raise Problem('forbidden',403)
                if role=='admin': store.sample()
                return self.send(200,store.snapshot(actor.get('tenant') if role=='tenant' else None))
            if not write and path=='/metrics':
                if role!='admin': raise Problem('forbidden',403)
                return self.send(200,store.metrics().encode(),'text/plain; version=0.0.4')
            if not write and path=='/api/results':
                if role=='agent': raise Problem('forbidden',403)
                from urllib.parse import parse_qs
                q=parse_qs(urlsplit(self.path).query)
                return self.send(200,store.results(q.get('model',[None])[0],q.get('kind',[None])[0]))
            if not write and path.startswith('/api/runs/') and path.endswith('/metrics'):
                if role=='agent': raise Problem('forbidden',403)
                return self.send(200,store.run_metrics(path.split('/')[3]))
            if not write and path=='/api/suites':
                if role=='agent': raise Problem('forbidden',403)
                return self.send(200,store.suites())
            if not write and path=='/api/telemetry':
                if role=='agent': raise Problem('forbidden',403)
                return self.send(200,store.telemetry() if role=='admin' else {})
            if not write and path=='/api/stream':
                if role=='agent': raise Problem('forbidden',403)
                return self.stream(store,actor.get('tenant') if role=='tenant' else None,role=='admin')
            if not write and path=='/api/history':
                if role!='admin': raise Problem('forbidden',403)
                store.sample(0)
                return self.send(200,store.history())
            if not write and path.startswith('/api/logs/'):
                if role=='agent': raise Problem('forbidden',403)
                ident=path.split('/')[-1]; state=store.snapshot(actor.get('tenant') if role=='tenant' else None)
                if not any(j['id']==ident for j in state['jobs']): raise Problem('not found',404)
                with store.lock: rows=[dict(r) for r in store.db.execute('SELECT * FROM logs WHERE job=? ORDER BY seq',(ident,))]
                return self.send(200,rows)
            if not write: raise Problem('not found',404)
            b=self.body()
            if path=='/api/jobs':
                if role not in ['admin','tenant']: raise Problem('forbidden',403)
                tenant=actor['tenant'] if role=='tenant' else b.pop('tenant','default')
                return self.send(201,{'id':store.submit(tenant,b)})
            if path=='/api/cancel':
                if role not in ['admin','tenant']: raise Problem('forbidden',403)
                store.cancel(b['id'],actor.get('tenant') if role=='tenant' else None)
            elif path=='/api/models/onboard':
                if role!='admin': raise Problem('forbidden',403)
                return self.send(201,{'job':store.onboard(b.pop('tenant','default'),b)})
            elif path=='/api/datasets/onboard':
                if role!='admin': raise Problem('forbidden',403)
                return self.send(201,{'job':store.onboard_dataset(b.pop('tenant','default'),b)})
            elif path=='/api/datasets':
                if role!='admin': raise Problem('forbidden',403)
                store.dataset(b.pop('id'),b)
            elif path=='/api/workflows':
                if role not in ['admin','tenant']: raise Problem('forbidden',403)
                return self.send(201,{'id':store.workflow(actor['tenant'] if role=='tenant' else b.pop('tenant','default'),b)})
            elif path=='/api/workflows/cancel':
                if role not in ['admin','tenant']: raise Problem('forbidden',403)
                store.cancel_workflow(b['id'],actor.get('tenant') if role=='tenant' else None)
            elif path=='/api/suites':
                if role!='admin': raise Problem('forbidden',403)
                store.suite(b['id'],b['cases'])
            elif path=='/api/priority':
                if role not in ['admin','tenant']: raise Problem('forbidden',403)
                store.priority(b['id'],b['priority'],actor.get('tenant') if role=='tenant' else None)
            elif path in ['/api/tenants','/api/drain','/api/models']:
                if role!='admin': raise Problem('forbidden',403)
                if path=='/api/tenants': store.tenant(b['id'],b['quota'],b.get('weight',1),b.get('budgetGiBHours'))
                elif path=='/api/drain':
                    if type(b['drain']) is not bool: raise Problem('drain must be boolean')
                    store.drain(b['id'],b['drain'])
                else: store.model(b.pop('id'),b)
            elif path.startswith('/api/agent/'):
                if role!='agent': raise Problem('forbidden',403)
                node=actor['node']; op=path.split('/')[-1]
                if op=='heartbeat': store.register(node,b)
                elif op=='claim': return self.send(200,{'job':store.claim(node)})
                elif op=='renew': return self.send(200,store.renew(node,b['id'],b['lease']))
                elif op=='complete':
                    if type(b['success']) is not bool: raise Problem('success must be boolean')
                    store.complete(node,b['id'],b['lease'],b['success'],b.get('result',{}))
                elif op=='logs': store.logs(node,b['id'],b['lease'],b['lines'])
                elif op=='log': store.log(node,b['id'],b['lease'],b['text'])
                else: raise Problem('not found',404)
            else: raise Problem('not found',404)
            self.send(200,{'ok':True})
        except Problem as e: self.send(e.status,{'error':e.message})
        except (KeyError,TypeError,ValueError): self.send(400,{'error':'invalid request fields'})
        except Exception: self.send(500,{'error':'internal error'})

def make_server(store,tokens,host='127.0.0.1',port=8443):
    server=ThreadingHTTPServer((host,port),API); server.store=store; server.tokens=tokens
    server.web=Path(__file__).resolve().parents[1]/'web'; server.gateway=Gateway(); return server

def main():
    p=argparse.ArgumentParser(); p.add_argument('--config',default='config.json'); p.add_argument('--db',default='gryvia.db'); p.add_argument('--host',default='127.0.0.1'); p.add_argument('--port',type=int,default=8443); p.add_argument('--cert'); p.add_argument('--inference-loopback',action='store_true',help='allow model replicas registered on 127.0.0.1 (serve jobs on the control-plane Mac)'); p.add_argument('--key'); a=p.parse_args()
    config=json.loads(Path(a.config).read_text()); tokens=config['tokens']
    if not tokens or any(len(t)<24 for t in tokens): p.error('tokens must have at least 24 characters')
    for actor in tokens.values():
        if actor.get('role') not in ['admin','tenant','agent']: p.error('invalid role')
        if actor['role']=='tenant' and not actor.get('tenant'): p.error('tenant required')
        if actor['role']=='agent' and not actor.get('node'): p.error('node required')
    if a.host not in ['127.0.0.1','localhost','::1'] and not (a.cert and a.key): p.error('remote bind requires --cert and --key')
    server=make_server(Store(a.db),tokens,a.host,a.port)
    server.gateway=Gateway(config.get("inferenceOrigins",[]),config.get("inferenceConcurrencyPerTenant",2),allow_loopback=a.inference_loopback or bool(config.get("inferenceAllowLoopback",False)))
    if a.cert:
        ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); ctx.minimum_version=ssl.TLSVersion.TLSv1_2; ctx.load_cert_chain(a.cert,a.key); server.socket=ctx.wrap_socket(server.socket,server_side=True)
    print('Gryvia Apple listening on',a.host,a.port,flush=True)
    try: server.serve_forever()
    except KeyboardInterrupt: pass
    finally: server.server_close()
if __name__=='__main__': main()
