# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Durable admission scheduler. All decisions and reservations share one SQLite transaction."""
import json, sqlite3, time, uuid, threading, math

RUNTIMES=['demo','mlx','llama','mps','coreml','download','bench','evaluate','quantize','train','serve','fuse']
KINDS=['inference','download','benchmark','evaluate','quantize','train','serve','dataset','fuse']
MODEL_STATES=['onboarding','ready','deployed','failed']
SOURCE_HOSTS=('huggingface.co','hf.co','cdn-lfs.huggingface.co','cdn-lfs-us-1.huggingface.co')

class Problem(Exception):
    def __init__(self, message, status=400): self.message, self.status = message, status

def positive(value, name):
    if isinstance(value, bool) or not isinstance(value, (int,float)) or not math.isfinite(value) or value <= 0:
        raise Problem(name+' must be positive')
    return value

class Store:
    def __init__(self, path, clock=time.time):
        self.clock=clock; self.lock=threading.RLock()
        self.db=sqlite3.connect(path, check_same_thread=False, isolation_level=None)
        self.db.row_factory=sqlite3.Row
        self.db.executescript('''PRAGMA journal_mode=WAL;
        CREATE TABLE IF NOT EXISTS tenants(id TEXT PRIMARY KEY, quota REAL NOT NULL, weight REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY, body TEXT NOT NULL, seen REAL NOT NULL, drain INTEGER DEFAULT 0);
        CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, tenant TEXT NOT NULL, body TEXT NOT NULL, state TEXT NOT NULL, created REAL NOT NULL, node TEXT, lease TEXT, expires REAL, attempt INTEGER DEFAULT 0, reason TEXT DEFAULT '', result TEXT DEFAULT '{}');
        CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT, at REAL, kind TEXT, subject TEXT, detail TEXT);
        CREATE TABLE IF NOT EXISTS logs(seq INTEGER PRIMARY KEY AUTOINCREMENT, job TEXT, at REAL, text TEXT);
        CREATE TABLE IF NOT EXISTS models(id TEXT PRIMARY KEY, body TEXT);
        CREATE TABLE IF NOT EXISTS history(at REAL PRIMARY KEY, online INTEGER, running INTEGER, queued INTEGER, reservedGiB REAL, budgetGiB REAL);
        CREATE TABLE IF NOT EXISTS datasets(id TEXT PRIMARY KEY, body TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS run_metrics(job TEXT, step INTEGER, key TEXT, value REAL, at REAL);
        CREATE INDEX IF NOT EXISTS run_metrics_job ON run_metrics(job, key, step);
        CREATE TABLE IF NOT EXISTS workflows(id TEXT PRIMARY KEY, body TEXT NOT NULL, at REAL);
        CREATE TABLE IF NOT EXISTS results(id INTEGER PRIMARY KEY AUTOINCREMENT, at REAL, job TEXT, model TEXT, kind TEXT, node TEXT, body TEXT);
        CREATE INDEX IF NOT EXISTS results_model ON results(model, kind, at);
        CREATE TABLE IF NOT EXISTS suites(id TEXT PRIMARY KEY, body TEXT NOT NULL, at REAL);
        CREATE TABLE IF NOT EXISTS node_telemetry(at REAL, node TEXT, util REAL, inUseGiB REAL, reservedGiB REAL);
        CREATE INDEX IF NOT EXISTS node_telemetry_node_at ON node_telemetry(node, at);
        CREATE TABLE IF NOT EXISTS decisions(seq INTEGER PRIMARY KEY AUTOINCREMENT, at REAL, job TEXT, node TEXT, kind TEXT, detail TEXT);
        CREATE TABLE IF NOT EXISTS usage(id INTEGER PRIMARY KEY AUTOINCREMENT, job TEXT, tenant TEXT, node TEXT, seconds REAL, gibSeconds REAL, at REAL);
        ''')
        cols=[r['name'] for r in self.db.execute('PRAGMA table_info(jobs)')]
        if 'started' not in cols: self.db.execute('ALTER TABLE jobs ADD COLUMN started REAL')
        if 'finished' not in cols: self.db.execute('ALTER TABLE jobs ADD COLUMN finished REAL')
        if 'budget' not in [r['name'] for r in self.db.execute('PRAGMA table_info(tenants)')]: self.db.execute('ALTER TABLE tenants ADD COLUMN budget REAL')
        self.db.execute('INSERT OR IGNORE INTO tenants(id,quota,weight) VALUES(?,?,?)',('default',32,1))
    def tx(self, fn):
        with self.lock:
            self.db.execute('BEGIN IMMEDIATE')
            try: result=fn(); self.db.execute('COMMIT'); return result
            except Exception: self.db.execute('ROLLBACK'); raise
    def event(self, kind, subject, detail=''):
        self.db.execute('INSERT INTO events(at,kind,subject,detail) VALUES(?,?,?,?)',(self.clock(),kind,subject,detail))
    def tenant(self, ident, quota, weight=1, budget=None):
        positive(quota,'quota'); positive(weight,'weight')
        if budget is not None: positive(budget,'budgetGiBHours')
        if not isinstance(ident,str) or not ident or len(ident)>80: raise Problem('invalid tenant')
        def run():
            self.db.execute('INSERT INTO tenants(id,quota,weight,budget) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET quota=excluded.quota,weight=excluded.weight,budget=COALESCE(excluded.budget,tenants.budget)',(ident,quota,weight,budget)); self.event('tenant.updated',ident)
        self.tx(run)
    def decide(self, job, node, kind, detail=''):
        self.db.execute('INSERT INTO decisions(at,job,node,kind,detail) VALUES(?,?,?,?,?)',(self.clock(),job,node,kind,detail))
        self.db.execute('DELETE FROM decisions WHERE seq NOT IN (SELECT seq FROM decisions ORDER BY seq DESC LIMIT 500)')
    def record_telemetry(self, node, body):
        """Per-heartbeat GPU sample. Utilisation is device-wide as reported by the Mac, not per job."""
        util=body.get('gpuUtilPct')
        if not isinstance(util,(int,float)) or isinstance(util,bool): return
        last=self.db.execute('SELECT MAX(at) AS a FROM node_telemetry WHERE node=?',(node,)).fetchone()['a']
        if last is not None and self.clock()-last<1.0: return
        reserved=sum(json.loads(j['body'])['memoryGiB'] for j in self.db.execute("SELECT body FROM jobs WHERE node=? AND state IN ('running','cancelling')",(node,)))
        in_use=body.get('gpuInUseMiB'); in_use=(in_use/1024.0) if isinstance(in_use,(int,float)) and not isinstance(in_use,bool) else 0.0
        self.db.execute('INSERT INTO node_telemetry VALUES(?,?,?,?,?)',(self.clock(),node,float(min(100,max(0,util))),in_use,reserved))
        self.db.execute('DELETE FROM node_telemetry WHERE node=? AND at NOT IN (SELECT at FROM node_telemetry WHERE node=? ORDER BY at DESC LIMIT 900)',(node,node))
    def telemetry(self, limit=120):
        limit=max(1,min(int(limit),900)); out={}
        with self.lock:
            for n in self.db.execute('SELECT id FROM nodes'):
                rows=self.db.execute('SELECT at,util,inUseGiB,reservedGiB FROM (SELECT * FROM node_telemetry WHERE node=? ORDER BY at DESC LIMIT ?) ORDER BY at',(n['id'],limit)).fetchall()
                out[n['id']]=[dict(r) for r in rows]
        return out
    def recent_decisions(self, limit=40):
        with self.lock: return [dict(r) for r in self.db.execute('SELECT * FROM (SELECT * FROM decisions ORDER BY seq DESC LIMIT ?) ORDER BY seq',(max(1,min(int(limit),200)),))]
    def register(self, ident, body):
        if not isinstance(body,dict): raise Problem('object required')
        if any(k in body for k in ['id','seen','online','drain']): raise Problem('reserved node fields')
        positive(body.get('budgetGiB'),'budgetGiB')
        slots=body.get('slots',1)
        if type(slots) is not int or slots<1 or slots>64: raise Problem('slots must be 1..64')
        if not isinstance(body.get('runtimes'),list) or not all(x in RUNTIMES for x in body['runtimes']): raise Problem('invalid runtimes')
        body['slots']=slots
        def run():
            self.db.execute('INSERT INTO nodes(id,body,seen) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,seen=excluded.seen',(ident,json.dumps(body),self.clock()))
            self.record_telemetry(ident,body)
        self.tx(run)
    def submit(self, tenant, body):
        if not isinstance(tenant,str) or not tenant: raise Problem('invalid tenant')
        if not isinstance(body,dict): raise Problem('object required')
        positive(body.get('memoryGiB'),'memoryGiB')
        if body.get('runtime') not in RUNTIMES: raise Problem('unsupported runtime')
        if body.get('kind','inference') not in KINDS: raise Problem('invalid kind')
        if 'modelId' in body and (not isinstance(body['modelId'],str) or not 0<len(body['modelId'])<=128): raise Problem('invalid modelId')
        if body.get('mode','exclusive') not in ['exclusive','shared']: raise Problem('invalid mode')
        for key, default, lo, hi in [('priority',50,0,100),('retries',0,0,10),('timeoutSeconds',3600,1,604800)]:
            v=body.get(key,default)
            if type(v) is not int or not lo<=v<=hi: raise Problem('invalid '+key)
            body[key]=v
        if 'node' in body and (not isinstance(body['node'],str) or len(body['node'])>80): raise Problem('invalid node selector')
        args=body.get('args',[])
        if not isinstance(args,list) or len(args)>64 or not all(isinstance(x,str) and len(x)<=4096 for x in args): raise Problem('invalid args')
        if not isinstance(body.get('model',''),str) or len(body.get('model',''))>256: raise Problem('invalid model')
        after=body.get('after',[])
        if not isinstance(after,list) or len(after)>16 or not all(isinstance(x,str) and len(x)<=64 for x in after): raise Problem('invalid after')
        if 'datasetId' in body and (not isinstance(body['datasetId'],str) or not 0<len(body['datasetId'])<=128): raise Problem('invalid datasetId')
        body['after']=after
        labels=body.get('labels',[])
        if not isinstance(labels,list) or len(labels)>8 or not all(isinstance(x,str) and 0<len(x)<=40 for x in labels): raise Problem('invalid labels')
        body['labels']=labels; body['args']=args; body['mode']=body.get('mode','exclusive'); ident=uuid.uuid4().hex
        def run():
            if not self.db.execute('SELECT 1 FROM tenants WHERE id=?',(tenant,)).fetchone(): raise Problem('unknown tenant')
            for dep in body['after']:
                if not self.db.execute('SELECT 1 FROM jobs WHERE id=? AND tenant=?',(dep,tenant)).fetchone(): raise Problem('unknown dependency '+dep[:12])
            self.db.execute('INSERT INTO jobs(id,tenant,body,state,created) VALUES(?,?,?,?,?)',(ident,tenant,json.dumps(body),'queued',self.clock())); self.event('job.submitted',ident); self.decide(ident,None,'queue','queued')
        self.tx(run); return ident
    def meter(self,j):
        if j['started'] is None: return
        seconds=max(0,min(self.clock(),j['expires'] or self.clock())-j['started'])
        self.db.execute('INSERT INTO usage(job,tenant,node,seconds,gibSeconds,at) VALUES(?,?,?,?,?,?)',(j['id'],j['tenant'],j['node'],seconds,seconds*json.loads(j['body'])['memoryGiB'],self.clock()))
    def expire(self):
        for row in self.db.execute("SELECT * FROM jobs WHERE state IN ('running','cancelling') AND expires<=?",(self.clock(),)).fetchall():
            self.meter(row)
            b=json.loads(row['body'])
            state='cancelled' if row['state']=='cancelling' and row['reason']!='job timeout' else ('queued' if row['attempt']<=b['retries'] else 'failed')
            self.db.execute('UPDATE jobs SET state=?,node=NULL,lease=NULL,expires=NULL,reason=?,finished=? WHERE id=?',(state,'lease expired; old worker must stop',self.clock() if state in ('cancelled','failed') else None,row['id'])); self.event('lease.expired',row['id']); self.decide(row['id'],row['node'],'expired','lease expired → '+state)
            if json.loads(row['body']).get('kind')=='serve': self.job_followup(row['id'],False,row['node'] or '')
        # Timeout requests a stop, but holds capacity until acknowledgement or lease expiry.
        for row in self.db.execute("SELECT * FROM jobs WHERE state='running'").fetchall():
            if self.clock() >= row['started'] + json.loads(row['body'])['timeoutSeconds']:
                self.db.execute("UPDATE jobs SET state='cancelling',reason='job timeout' WHERE id=?",(row['id'],))
                self.event('job.timeout',row['id'])
    def claim(self, node):
        def run():
            self.expire(); n=self.db.execute('SELECT * FROM nodes WHERE id=?',(node,)).fetchone()
            if not n: raise Problem('register first',409)
            if n['drain'] or self.clock()-n['seen']>30: return None
            nb=json.loads(n['body'])
            if nb.get('thermal') in ['serious','critical'] or nb.get('pressure')=='critical': return None
            active=self.db.execute("SELECT * FROM jobs WHERE state IN ('running','cancelling')").fetchall()
            local=[j for j in active if j['node']==node]
            if len(local)>=nb['slots'] or any(json.loads(j['body'])['mode']=='exclusive' for j in local): return None
            tenants={r['id']:dict(r) for r in self.db.execute('SELECT * FROM tenants')}
            used={k:sum(json.loads(j['body'])['memoryGiB'] for j in active if j['tenant']==k) for k in tenants}
            pending=self.db.execute("SELECT * FROM jobs WHERE state='queued'").fetchall()
            pending.sort(key=lambda j:(-json.loads(j['body'])['priority'],used[j['tenant']]/tenants[j['tenant']]['quota']/tenants[j['tenant']]['weight'],j['created'],j['id']))
            for j in pending:
                b=json.loads(j['body']); reason=''
                deps=[self.db.execute('SELECT state FROM jobs WHERE id=?',(d,)).fetchone() for d in b.get('after',[])]
                if any(d is None or d['state'] in ('failed','cancelled') for d in deps):
                    self.db.execute("UPDATE jobs SET state='cancelled',reason='dependency failed',finished=? WHERE id=?",(self.clock(),j['id'])); self.event('job.cancelled',j['id'],'dependency failed'); self.decide(j['id'],None,'cancel','a dependency failed'); continue
                if any(d['state']!='succeeded' for d in deps): reason='waiting for dependency'
                elif b['runtime'] not in nb['runtimes']: reason='runtime unavailable'
                elif b.get('node') and b['node']!=node: reason='node selector'
                elif used[j['tenant']]+b['memoryGiB']>tenants[j['tenant']]['quota']: reason='tenant memory quota'
                elif b['mode']=='exclusive' and local: reason='waiting for exclusive worker'
                elif sum(json.loads(x['body'])['memoryGiB'] for x in local)+b['memoryGiB']>nb['budgetGiB']: reason='node memory reservation budget'
                elif nb.get('pressure')=='warning': reason='memory pressure'
                if reason:
                    if j['reason']!=reason: self.decide(j['id'],node,'wait',reason)
                    self.db.execute('UPDATE jobs SET reason=? WHERE id=?',(reason,j['id'])); continue
                lease=uuid.uuid4().hex
                self.db.execute("UPDATE jobs SET state='running',node=?,lease=?,expires=?,started=?,attempt=attempt+1,reason='' WHERE id=?",(node,lease,self.clock()+20,self.clock(),j['id'])); self.event('job.claimed',j['id'],node); self.decide(j['id'],node,'admit','admitted on '+node)
                return dict(id=j['id'],lease=lease,spec=self.resolve(b),expires=self.clock()+20)
            return None
        return self.tx(run)
    def resolve(self, spec):
        """Fill {{model.path}} / {{model.id}} / {{dataset.path}} in args at claim time, so pipeline steps can use earlier outputs."""
        out=dict(spec); vals={}
        m=self.db.execute('SELECT body FROM models WHERE id=?',(spec.get('modelId',''),)).fetchone() if spec.get('modelId') else None
        if m: mb=json.loads(m['body']); vals.update({'{{model.path}}':mb.get('path',''),'{{model.id}}':spec['modelId'],'{{model.source}}':mb.get('source','')})
        d=self.db.execute('SELECT body FROM datasets WHERE id=?',(spec.get('datasetId',''),)).fetchone() if spec.get('datasetId') else None
        if d: vals.update({'{{dataset.path}}':json.loads(d['body']).get('path',''),'{{dataset.id}}':spec['datasetId']})
        if vals:
            args=[]
            for x in spec.get('args',[]):
                if isinstance(x,str):
                    for k,v in vals.items(): x=x.replace(k,v)
                args.append(x)
            out['args']=args
        return out
    def renew(self, node, ident, lease):
        def run():
            self.expire(); j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
            if not j or j['node']!=node or j['lease']!=lease or j['state'] not in ['running','cancelling']: raise Problem('lease lost; stop worker',409)
            deadline=j['started']+json.loads(j['body'])['timeoutSeconds']
            expires=min(self.clock()+20,deadline+20)
            self.db.execute('UPDATE jobs SET expires=? WHERE id=?',(expires,ident))
            return {'cancel':j['state']=='cancelling','expires':expires,'reason':j['reason']}
        return self.tx(run)
    def complete(self,node,ident,lease,success,result):
        def run():
            self.expire(); j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
            if not j or j['node']!=node or j['lease']!=lease or j['state'] not in ['running','cancelling']: raise Problem('stale completion',409)
            self.meter(j)
            b=json.loads(j['body']); timed_out=j['reason']=='job timeout'
            state='cancelled' if j['state']=='cancelling' and not timed_out else ('succeeded' if success and not timed_out else ('queued' if j['attempt']<=b['retries'] else 'failed'))
            self.db.execute('UPDATE jobs SET state=?,result=?,lease=NULL,node=NULL,expires=NULL,finished=? WHERE id=?',(state,json.dumps(result),self.clock() if state!='queued' else None,ident)); self.event('job.'+state,ident); self.decide(ident,node,state,'finished on '+node)
            if state in ('succeeded','failed','cancelled'): self.job_followup(ident,state=='succeeded',node)
        self.tx(run)
    def cancel(self,ident,tenant=None):
        def run():
            j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
            if not j or (tenant and tenant!=j['tenant']): raise Problem('job not found',404)
            if j['state'] in ['queued','running']:
                self.db.execute('UPDATE jobs SET state=?,finished=? WHERE id=?',('cancelled' if j['state']=='queued' else 'cancelling',self.clock() if j['state']=='queued' else None,ident)); self.event('job.cancel',ident); self.decide(ident,j['node'],'cancel','cancel requested')
        self.tx(run)
    def log(self,node,ident,lease,line):
        self.logs(node,ident,lease,[line])
    def logs(self,node,ident,lease,lines):
        if not isinstance(lines,list) or not 1<=len(lines)<=100: raise Problem('logs must contain 1..100 lines')
        if not all(isinstance(line,str) and len(line.encode('utf-8'))<=8192 for line in lines): raise Problem('log too large')
        if sum(len(line.encode('utf-8')) for line in lines)>49152: raise Problem('log batch too large')
        def run():
            j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
            if not j or j['node']!=node or j['lease']!=lease or (j['expires'] or 0)<=self.clock(): raise Problem('stale log',409)
            self.db.executemany('INSERT INTO logs(job,at,text) VALUES(?,?,?)',[(ident,self.clock(),line) for line in lines])
            self.db.execute('DELETE FROM logs WHERE job=? AND seq NOT IN (SELECT seq FROM logs WHERE job=? ORDER BY seq DESC LIMIT 2000)',(ident,ident))
            spec=json.loads(j['body'])
            for line in lines:
                if not line.startswith('{'): continue
                try: obj=json.loads(line)
                except ValueError: continue
                if not isinstance(obj,dict): continue
                if obj.get('kind')=='metric' and type(obj.get('step')) is int:
                    for key in ('trainLoss','valLoss','lr','tokensPerSec','itPerSec','peakGB'):
                        v=obj.get(key)
                        if isinstance(v,(int,float)) and not isinstance(v,bool): self.db.execute('INSERT INTO run_metrics VALUES(?,?,?,?,?)',(ident,obj['step'],key,float(v),self.clock()))
                elif obj.get('kind')=='endpoint' and spec.get('kind')=='serve' and isinstance(obj.get('url'),str) and spec.get('modelId'):
                    row=self.db.execute('SELECT body FROM models WHERE id=?',(spec['modelId'],)).fetchone()
                    if row:
                        m=json.loads(row['body'])
                        m['endpoints']=[obj['url']]; m['status']='deployed'; m['deployJob']=ident
                        if m.get('runtime')=='mlx': m['workerModel']='default_model'   # mlx_lm.server serves the model it was started with under this name   # one serve job per model: the newest replaces any stale entry
                        self.db.execute('UPDATE models SET body=? WHERE id=?',(json.dumps(m),spec['modelId'])); self.event('model.deployed',spec['modelId'],obj['url'])
        self.tx(run)
    def drain(self,node,value):
        def run():
            if not self.db.execute('SELECT 1 FROM nodes WHERE id=?',(node,)).fetchone(): raise Problem('node not found',404)
            self.db.execute('UPDATE nodes SET drain=? WHERE id=?',(int(value),node)); self.event('node.drain',node,str(value))
        self.tx(run)
    def model(self,ident,body):
        if not isinstance(ident,str) or not ident or len(ident)>128 or not isinstance(body,dict): raise Problem('invalid model')
        positive(body.get('memoryGiB'),'memoryGiB')
        for field in ['tenants','endpoints']:
            if field in body and (not isinstance(body[field],list) or len(body[field])>64 or not all(isinstance(v,str) for v in body[field])): raise Problem('invalid model '+field)
        if body.get('status','ready') not in MODEL_STATES: raise Problem('invalid model status')
        for field in ['source','revision','quant','license','path','notes','sha256']:
            if field in body and (not isinstance(body[field],str) or len(body[field])>2048): raise Problem('invalid model '+field)
        if 'paramsB' in body: positive(body['paramsB'],'paramsB')
        body.setdefault('status','ready'); body['updated']=self.clock()
        def run():
            self.db.execute('INSERT OR REPLACE INTO models VALUES(?,?)',(ident,json.dumps(body))); self.event('model.saved',ident,body['status'])
        self.tx(run)
    @staticmethod
    def estimate_memory_gib(params_b,quant=''):
        """Rough resident size: params × bits/8 × 1.15 plus ~0.75 GiB for context and runtime. An estimate, not a measurement."""
        q=(quant or '').lower(); bits=16
        for key,b in (('q2',2.8),('q3',3.6),('q4',4.8),('q5',5.7),('q6',6.6),('q8',8.5),('int4',4.5),('4bit',4.5),('8bit',8.5),('f16',16),('bf16',16),('fp16',16),('f32',32)):
            if key in q: bits=b; break
        return round(params_b*1e9*bits/8*1.15/1024**3+0.75,1)
    def onboard(self,tenant,body):
        """Create a model record in `onboarding` state and queue the download job that fills it."""
        if not isinstance(body,dict): raise Problem('object required')
        ident=body.get('id'); src=body.get('source')
        if not isinstance(ident,str) or not 0<len(ident)<=128 or any(c in ident for c in '\\ \n'): raise Problem('invalid model id')
        if not isinstance(src,dict): raise Problem('source required')
        kind=src.get('kind','hf')
        if kind=='hf':
            repo,file_=src.get('repo'),src.get('file')
            parts=repo.split('/') if isinstance(repo,str) else []
            if len(parts)!=2 or not all(p and not p.startswith('.') and '..' not in p and all(ch.isalnum() or ch in '-_.' for ch in p) for p in parts): raise Problem('invalid Hugging Face repo')
            if not isinstance(file_,str) or not file_ or '..' in file_ or file_.startswith('/') or len(file_)>256: raise Problem('invalid file')
            args=['--repo',repo,'--file',file_,'--revision',str(src.get('revision','main'))]
        elif kind=='url':
            from urllib.parse import urlsplit
            u=urlsplit(str(src.get('url','')))
            if u.scheme!='https' or u.hostname not in SOURCE_HOSTS: raise Problem('url must be https on '+', '.join(SOURCE_HOSTS))
            args=['--url',src['url']]
        else: raise Problem('unsupported source kind')
        max_gib=src.get('maxGiB',20)
        positive(max_gib,'maxGiB'); args+=['--max-gib',str(min(float(max_gib),200))]
        params=body.get('paramsB'); quant=body.get('quant','')
        mem=body.get('memoryGiB') or (self.estimate_memory_gib(params,quant) if params else 8.0)
        origin=(repo+'/'+file_) if kind=='hf' else src['url']
        rec=dict(runtime=body.get('runtime','llama'),memoryGiB=mem,status='onboarding',source=origin,revision=str(src.get('revision','main')),quant=quant,license=body.get('license',''))
        if params: rec['paramsB']=params
        if rec['runtime'] not in ['mlx','llama','coreml','mps']: raise Problem('unsupported model runtime')
        self.model(ident,rec)
        spec=dict(runtime='download',kind='download',modelId=ident,memoryGiB=0.5,priority=60,mode='shared',timeoutSeconds=7200,retries=0,args=args,labels=['onboard'],model='download '+ident)
        if body.get('node'): spec['node']=body['node']
        job=self.submit(tenant,spec)
        self.tx(lambda:self.event('model.onboard',ident,job))
        return job
    def onboard_dataset(self,tenant,body):
        if not isinstance(body,dict) or not isinstance(body.get('source'),dict): raise Problem('source required')
        ident=body.get('id'); src=body['source']
        if not isinstance(ident,str) or not 0<len(ident)<=128 or any(c in ident for c in '\\ \n'): raise Problem('invalid dataset id')
        repo,file_=src.get('repo'),src.get('file')
        parts=repo.split('/') if isinstance(repo,str) else []
        if len(parts)!=2 or not all(p and not p.startswith('.') and '..' not in p and all(ch.isalnum() or ch in '-_.' for ch in p) for p in parts): raise Problem('invalid Hugging Face dataset repo')
        if not isinstance(file_,str) or not file_ or '..' in file_ or file_.startswith('/') or len(file_)>256: raise Problem('invalid file')
        max_gib=src.get('maxGiB',2); positive(max_gib,'maxGiB')
        self.dataset(ident,dict(status='onboarding',source=f'datasets/{repo}/{file_}'))
        spec=dict(runtime='download',kind='dataset',datasetId=ident,memoryGiB=0.5,priority=60,mode='shared',timeoutSeconds=3600,retries=0,model='dataset '+ident,
                  args=['--dataset','--repo',repo,'--file',file_,'--revision',str(src.get('revision','main')),'--max-gib',str(min(float(max_gib),50))],labels=['dataset'])
        if body.get('node'): spec['node']=body['node']
        return self.submit(tenant,spec)
    def job_followup(self,ident,success,node=''):
        """When a model-related job finishes, fold its structured `result` log line into the registry and results table."""
        j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
        if not j: return
        spec=json.loads(j['body']); kind=spec.get('kind','inference'); model=spec.get('modelId')
        result=None
        for row in self.db.execute('SELECT text FROM logs WHERE job=? ORDER BY seq DESC LIMIT 200',(ident,)):
            try: obj=json.loads(row['text'])
            except ValueError: continue
            if isinstance(obj,dict) and obj.get('kind')=='result': result=obj; break
        if kind=='dataset':
            ds=spec.get('datasetId')
            if ds and not (success and result):
                self.db.execute('INSERT OR REPLACE INTO datasets VALUES(?,?)',(ds,json.dumps(dict(status='failed',updated=self.clock()))))
            if ds and success and result:
                self.db.execute('INSERT OR REPLACE INTO datasets VALUES(?,?)',(ds,json.dumps(dict(path=result.get('path',''),sha256=result.get('sha256',''),bytes=result.get('bytes',0),rows=result.get('rows'),source=result.get('source',''),node=node,status='ready',updated=self.clock()))))
                self.event('dataset.ready',ds,ident)
            return
        if kind=='inference' or not model: return
        if result is not None and kind=='evaluate' and 'cases' not in result:
            cases=[]
            for row in self.db.execute("SELECT text FROM logs WHERE job=? AND text LIKE '{\"kind\": \"case\"%' ORDER BY seq",(ident,)):
                try: obj=json.loads(row['text'])
                except ValueError: continue
                obj.pop('kind',None); cases.append(obj)
            if cases: result['cases']=cases[:200]
        node=node or j['node'] or ''
        self.db.execute('INSERT INTO results(at,job,model,kind,node,body) VALUES(?,?,?,?,?,?)',(self.clock(),ident,model,kind,node,json.dumps(dict(success=bool(success and result),**(result or {})))))
        row=self.db.execute('SELECT body FROM models WHERE id=?',(model,)).fetchone()
        if kind=='download' and row:
            m=json.loads(row['body'])
            if success and result:
                m.update(status='ready',path=result.get('path',''),sha256=result.get('sha256',''),sizeBytes=result.get('bytes',0),node=node)
            else: m['status']='failed'
            m['updated']=self.clock(); self.db.execute('UPDATE models SET body=? WHERE id=?',(json.dumps(m),model))
            self.event('model.'+m['status'],model,ident)
        if kind=='train' and success and result and row:
            base=json.loads(row['body']); tag=ident[:6]
            child=dict(runtime='mlx',memoryGiB=base.get('memoryGiB',4),status='ready',path=result.get('adapterPath',''),parent=model,source=base.get('source',''),quant=base.get('quant',''),
                       kind='lora-adapter',dataset=spec.get('datasetId',''),trainJob=ident,finalValLoss=result.get('finalValLoss'),finalTrainLoss=result.get('finalTrainLoss'),node=node,updated=self.clock())
            self.db.execute('INSERT OR REPLACE INTO models VALUES(?,?)',(f'{model}-lora-{tag}',json.dumps(child))); self.event('model.adapter',f'{model}-lora-{tag}',ident)
        if kind=='serve' and row:
            m=json.loads(row['body']); m.pop('deployJob',None); m['endpoints']=[]; m['status']='ready' if m.get('path') else m.get('status','ready')
            self.db.execute('UPDATE models SET body=? WHERE id=?',(json.dumps(m),model)); self.event('model.undeployed',model,ident)
        if kind=='fuse' and success and result and row:
            parent=json.loads(row['body'])
            child=dict(runtime='mlx',memoryGiB=parent.get('memoryGiB',4),status='ready',path=result.get('path',''),sizeBytes=result.get('bytes',0),parent=model,
                       source=parent.get('source',''),quant=parent.get('quant',''),kind='fused',fusedFrom=parent.get('trainJob',''),dataset=parent.get('dataset',''),node=node,updated=self.clock())
            fid=f'{model}-fused'
            self.db.execute('INSERT OR REPLACE INTO models VALUES(?,?)',(fid,json.dumps(child))); self.event('model.fused',fid,ident)
        if kind=='quantize' and success and result and row:
            parent=json.loads(row['body']); q=result.get('quant','quant')
            child=dict(parent,status='ready',path=result.get('path',''),sha256=result.get('sha256',''),sizeBytes=result.get('bytes',0),quant=q,parent=model,updated=self.clock())
            child.pop('endpoints',None); child['memoryGiB']=round(parent['memoryGiB']*self.estimate_memory_gib(1,q)/max(self.estimate_memory_gib(1,parent.get('quant','')),0.1),1) if parent.get('paramsB') is None else self.estimate_memory_gib(parent['paramsB'],q)
            self.db.execute('INSERT OR REPLACE INTO models VALUES(?,?)',(f'{model}-{q}',json.dumps(child))); self.event('model.quantized',f'{model}-{q}',ident)
    def results(self,model=None,kind=None,limit=100):
        q='SELECT * FROM results WHERE 1=1'; a=[]
        if model: q+=' AND model=?'; a.append(model)
        if kind: q+=' AND kind=?'; a.append(kind)
        q+=' ORDER BY id DESC LIMIT ?'; a.append(max(1,min(int(limit),500)))
        with self.lock: return [dict(r,body=json.loads(r['body'])) for r in self.db.execute(q,a)]
    def run_metrics(self,job):
        with self.lock:
            out={}
            for r in self.db.execute('SELECT step,key,value FROM run_metrics WHERE job=? ORDER BY step',(job,)): out.setdefault(r['key'],[]).append([r['step'],r['value']])
            return out
    def dataset(self,ident,body):
        if not isinstance(ident,str) or not 0<len(ident)<=128 or not isinstance(body,dict): raise Problem('invalid dataset')
        for f in ('path','source','notes'):
            if f in body and (not isinstance(body[f],str) or len(body[f])>2048): raise Problem('invalid '+f)
        body.setdefault('status','ready'); body['updated']=self.clock()
        self.tx(lambda:self.db.execute('INSERT OR REPLACE INTO datasets VALUES(?,?)',(ident,json.dumps(body))))
    def workflow(self,tenant,body):
        """Create a chain/DAG of jobs. Step N may list `dependsOn` step names; each becomes `after` job ids."""
        if not isinstance(body,dict) or not isinstance(body.get('steps'),list) or not 1<=len(body['steps'])<=12: raise Problem('workflow needs 1..12 steps')
        name=str(body.get('name') or 'workflow')[:80]; ids={}; steps=[]
        for i,st in enumerate(body['steps']):
            if not isinstance(st,dict) or not isinstance(st.get('spec'),dict): raise Problem('each step needs a spec')
            sname=str(st.get('name') or f'step{i+1}')[:60]
            if sname in ids: raise Problem('duplicate step name')
            deps=st.get('dependsOn',[ list(ids)[-1] ] if ids else [])
            if not isinstance(deps,list) or any(d not in ids for d in deps): raise Problem('dependsOn must name earlier steps')
            spec=dict(st['spec']); spec['after']=[ids[d] for d in deps]; spec.setdefault('labels',[]); spec['labels']=list(spec['labels'])[:7]+['wf:'+name[:30]]
            ids[sname]=self.submit(tenant,spec); steps.append(dict(name=sname,job=ids[sname],dependsOn=deps))
        wid=uuid.uuid4().hex[:12]
        self.tx(lambda:self.db.execute('INSERT INTO workflows VALUES(?,?,?)',(wid,json.dumps(dict(name=name,steps=steps,tenant=tenant)),self.clock())))
        return wid
    def cancel_workflow(self,wid,tenant=None):
        row=self.db.execute('SELECT body FROM workflows WHERE id=?',(wid,)).fetchone()
        if not row: raise Problem('workflow not found',404)
        w=json.loads(row['body'])
        if tenant and tenant!=w['tenant']: raise Problem('workflow not found',404)
        for s in w['steps']:
            try: self.cancel(s['job'],tenant)
            except Problem: pass
    def suite(self,ident,cases):
        if not isinstance(ident,str) or not 0<len(ident)<=80: raise Problem('invalid suite id')
        if not isinstance(cases,list) or not 1<=len(cases)<=200: raise Problem('suite needs 1..200 cases')
        for c in cases:
            if not isinstance(c,dict) or not isinstance(c.get('prompt'),str) or not 0<len(c['prompt'])<=4000: raise Problem('each case needs a prompt')
            if not any(k in c for k in ('contains','equals','regex')): raise Problem('each case needs contains, equals or regex')
        self.tx(lambda:self.db.execute('INSERT OR REPLACE INTO suites VALUES(?,?,?)',(ident,json.dumps(cases),self.clock())))
    def suites(self):
        with self.lock: return {r['id']:json.loads(r['body']) for r in self.db.execute('SELECT * FROM suites')}
    def priority(self,ident,value,tenant=None):
        if type(value) is not int or not 0<=value<=100: raise Problem('invalid priority')
        def run():
            j=self.db.execute('SELECT * FROM jobs WHERE id=?',(ident,)).fetchone()
            if not j or (tenant and tenant!=j['tenant']): raise Problem('job not found',404)
            if j['state']!='queued': raise Problem('only queued jobs can change priority',409)
            b=json.loads(j['body']); b['priority']=value
            self.db.execute('UPDATE jobs SET body=? WHERE id=?',(json.dumps(b),ident)); self.event('job.priority',ident,str(value))
        self.tx(run)
    def fleet_totals(self):
        now=self.clock(); nodes=self.db.execute('SELECT * FROM nodes').fetchall()
        online=[n for n in nodes if now-n['seen']<=30]
        budget=sum(json.loads(n['body'])['budgetGiB'] for n in online)
        states={s:0 for s in ('queued','running','cancelling','succeeded','failed','cancelled')}
        reserved=0.0
        for j in self.db.execute('SELECT state,body FROM jobs'):
            states[j['state']]=states.get(j['state'],0)+1
            if j['state'] in ('running','cancelling'): reserved+=json.loads(j['body'])['memoryGiB']
        return dict(nodes=len(nodes),online=len(online),budget=budget,reserved=reserved,states=states)
    def sample(self,every=10):
        """Bounded fleet history ring for sparklines; at most one row per `every` seconds."""
        def run():
            now=self.clock(); last=self.db.execute('SELECT MAX(at) AS a FROM history').fetchone()['a']
            if last is not None and now-last<every: return
            t=self.fleet_totals()
            self.db.execute('INSERT OR REPLACE INTO history VALUES(?,?,?,?,?,?)',(now,t['online'],t['states']['running'],t['states']['queued'],t['reserved'],t['budget']))
            self.db.execute('DELETE FROM history WHERE at NOT IN (SELECT at FROM history ORDER BY at DESC LIMIT 2000)')
        self.tx(run)
    def history(self,limit=240):
        limit=max(1,min(int(limit),2000))
        with self.lock: return [dict(r) for r in self.db.execute('SELECT * FROM (SELECT * FROM history ORDER BY at DESC LIMIT ?) ORDER BY at',(limit,))]
    def metrics(self):
        """Prometheus text exposition. Reservation figures are admission bookkeeping, not measured GPU use."""
        with self.lock:
            t=self.fleet_totals(); out=[]
            def g(name,help_,value,labels=''):
                out.append(f'# HELP {name} {help_}'); out.append(f'# TYPE {name} gauge'); out.append(f'{name}{labels} {value}')
            g('gryvia_nodes_registered','Registered Mac workers',t['nodes'])
            g('gryvia_nodes_online','Workers seen within 30s',t['online'])
            g('gryvia_budget_gib','Total reservation budget of online workers (GiB)',t['budget'])
            g('gryvia_reserved_gib','Memory reserved by running jobs (GiB, not measured usage)',t['reserved'])
            out.append('# HELP gryvia_jobs Jobs by state'); out.append('# TYPE gryvia_jobs gauge')
            for s,v in t['states'].items(): out.append(f'gryvia_jobs{{state="{s}"}} {v}')
            return '\n'.join(out)+'\n'
    def snapshot(self,tenant=None):
        def run():
            self.expire()
            jobs=[]
            for row in self.db.execute('SELECT * FROM jobs ORDER BY created DESC LIMIT 1000'):
                j=dict(row)
                if tenant and j['tenant']!=tenant: continue
                j['spec']=json.loads(j.pop('body')); j['result']=json.loads(j['result']); j.pop('lease'); jobs.append(j)
            return {'jobs':jobs,'nodes':[] if tenant else [dict(id=r['id'],seen=r['seen'],drain=bool(r['drain']),online=self.clock()-r['seen']<=30,**json.loads(r['body'])) for r in self.db.execute('SELECT * FROM nodes')], 'tenants':[dict(r) for r in self.db.execute('SELECT * FROM tenants') if not tenant or r['id']==tenant], 'models':[dict(id=r['id'],**json.loads(r['body'])) for r in self.db.execute('SELECT * FROM models') if not tenant or not json.loads(r['body']).get('tenants') or tenant in json.loads(r['body'])['tenants']], 'events':[dict(r) for r in self.db.execute('SELECT * FROM events ORDER BY seq DESC LIMIT 100')] if not tenant else [], 'usage':[dict(r) for r in self.db.execute('SELECT tenant,SUM(seconds) AS workerSeconds,SUM(gibSeconds) AS reservedGiBSeconds FROM usage GROUP BY tenant') if not tenant or r['tenant']==tenant], 'decisions':[] if tenant else self.recent_decisions(40),'datasets':[dict(id=r['id'],**json.loads(r['body'])) for r in self.db.execute('SELECT * FROM datasets')],'workflows':[dict(id=r['id'],at=r['at'],**json.loads(r['body'])) for r in self.db.execute('SELECT * FROM workflows ORDER BY at DESC LIMIT 50') if not tenant or json.loads(r['body'])['tenant']==tenant],'time':self.clock()}
        return self.tx(run)
