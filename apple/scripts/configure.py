#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Generate local configs without shipping fixed or demo credentials."""
import argparse,json,secrets,os,sys
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--output',default='local');p.add_argument('--server',default='http://127.0.0.1:8443');p.add_argument('--node',default='mini-01');p.add_argument('--budget',type=float,default=8);p.add_argument('--python',default=sys.executable);p.add_argument('--mlx-python');p.add_argument('--llama-cli');a=p.parse_args()
root=Path(__file__).resolve().parents[1];out=Path(a.output).resolve();out.mkdir(parents=True,exist_ok=True)
for name in ['config.json','agent.json','credentials.env']:
    if (out/name).exists():p.error('Refusing to overwrite existing configuration: '+name)
admin,agent,tenant=[secrets.token_urlsafe(32) for _ in range(3)]
runtimes={'demo':{'executable':str(Path(a.python).resolve()),'prefix':[str(root/'runtimes/demo.py')]}}
if a.mlx_python:runtimes['mlx']={'executable':str(Path(a.mlx_python).resolve()),'prefix':[str(root/'runtimes/mlx_generate.py')]}
if a.llama_cli:runtimes['llama']={'executable':str(Path(a.llama_cli).resolve()),'prefix':[]}
files={'config.json':json.dumps({'tokens':{admin:{'role':'admin'},agent:{'role':'agent','node':a.node},tenant:{'role':'tenant','tenant':'default'}}},indent=2),'agent.json':json.dumps({'server':a.server,'budgetGiB':a.budget,'runtimes':runtimes},indent=2),'credentials.env':f'export GRYVIA_ADMIN_TOKEN={admin}\nexport GRYVIA_AGENT_TOKEN={agent}\nexport GRYVIA_TENANT_TOKEN={tenant}\n'}
for name,text in files.items():
    fd=os.open(out/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f:f.write(text+'\n')
print('Created configuration in',out,'; tokens are in credentials.env')
