# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Bounded non-streaming OpenAI-compatible routing to administrator-allowlisted workers."""
import json, threading, time, urllib.request, urllib.error, urllib.parse
from server.core import Problem

class Gateway:
    def __init__(self, allowed_origins=(), max_inflight=2, allow_loopback=False):
        if type(max_inflight) is not int or not 1<=max_inflight<=64: raise Problem('invalid inference concurrency')
        if not isinstance(allowed_origins,(list,tuple)) or not all(isinstance(x,str) for x in allowed_origins): raise Problem('invalid inference origins')
        self.allowed=set(allowed_origins);self.allow_loopback=bool(allow_loopback);self.limit=max_inflight;self.lock=threading.Lock();self.active={};self.calls={}
    def chat(self, store, tenant, body):
        if body.get('stream'):raise Problem('streaming is not supported; use stream=false')
        model=body.get('model');messages=body.get('messages');maximum=body.get('max_tokens',256)
        if not isinstance(model,str) or not isinstance(messages,list) or not 1<=len(messages)<=128:raise Problem('model and messages required')
        if type(maximum) is not int or not 1<=maximum<=4096:raise Problem('max_tokens must be 1..4096')
        if not all(isinstance(m,dict) and m.get('role') in ['system','user','assistant'] and isinstance(m.get('content'),str) for m in messages):raise Problem('invalid messages')
        if sum(len(m['content']) for m in messages)>32768:raise Problem('input too long',413)
        catalog=next((m for m in store.snapshot(tenant)['models'] if m['id']==model),None)
        if not catalog:raise Problem('model not found',404)
        if catalog.get('tenants') and tenant not in catalog['tenants']:raise Problem('model not found',404)
        endpoints=catalog.get('endpoints',[])
        if not isinstance(endpoints,list):raise Problem('invalid model endpoints',503)
        candidates=[]
        for e in endpoints:
            if not isinstance(e,str):continue
            u=urllib.parse.urlsplit(e)
            origin=f'{u.scheme}://{u.netloc}'
            loopback=self.allow_loopback and u.scheme=='http' and u.hostname in ('127.0.0.1','localhost','::1')
            if (origin in self.allowed or loopback) and u.scheme in ['http','https'] and not u.username and not u.password and not u.query and not u.fragment and u.path in ['','/']:candidates.append(e.rstrip('/'))
        if not candidates:raise Problem('no allowlisted inference replica',503)
        with self.lock:
            if self.calls.get(tenant,0)>=self.limit:raise Problem('tenant inference concurrency exceeded',429)
            endpoint=min(candidates,key=lambda e:self.active.get(e,0))
            self.active[endpoint]=self.active.get(endpoint,0)+1;self.calls[tenant]=self.calls.get(tenant,0)+1
        try:
            request={'model':catalog.get('workerModel',model),'messages':messages,'max_tokens':maximum,'stream':False}
            req=urllib.request.Request(endpoint+'/v1/chat/completions',data=json.dumps(request).encode(),headers={'Content-Type':'application/json'})
            # Never forward the caller's platform token to a model server.
            # Redirects are forbidden so an allowlisted origin cannot redirect to another host.
            class NoRedirect(urllib.request.HTTPRedirectHandler):
                def redirect_request(self,*args):return None
            try:
                with urllib.request.build_opener(NoRedirect()).open(req,timeout=30) as r:
                    data=r.read(4*1024*1024+1)
                    if len(data)>4*1024*1024:raise Problem('worker response too large',502)
                    result=json.loads(data)
            except (urllib.error.URLError,ValueError):raise Problem('inference replica failed',502)
            if not isinstance(result,dict) or not isinstance(result.get('choices'),list):raise Problem('invalid inference response',502)
            with store.lock:store.event('inference.completed',tenant,model)
            return result
        finally:
            with self.lock:self.active[endpoint]-=1;self.calls[tenant]-=1
