#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Install a per-user launchd agent without sudo. Run from a logged-in Mac session."""
import argparse,os,plistlib,subprocess,sys
from pathlib import Path
if sys.platform!='darwin':raise SystemExit('macOS required')
p=argparse.ArgumentParser();p.add_argument('--binary',required=True);p.add_argument('--config',required=True);p.add_argument('--uninstall',action='store_true');p.add_argument('--replace',action='store_true',help='reinstall over an existing agent');a=p.parse_args()
label='dev.zyvor.gryvia.agent';plist=Path.home()/'Library/LaunchAgents'/f'{label}.plist';domain=f'gui/{os.getuid()}'
if a.uninstall:
    subprocess.run(['launchctl','bootout',domain,str(plist)],check=False);plist.unlink(missing_ok=True);raise SystemExit(0)
token=os.getenv('GRYVIA_AGENT_TOKEN','')
if len(token)<24:p.error('set GRYVIA_AGENT_TOKEN')
binary=Path(a.binary).resolve();config=Path(a.config).resolve()
if not os.access(binary,os.X_OK) or not config.is_file():p.error('binary/config unavailable')
logs=Path.home()/'Library/Logs/Gryvia';logs.mkdir(parents=True,exist_ok=True);plist.parent.mkdir(parents=True,exist_ok=True)
if plist.exists():
    if not a.replace:p.error('already installed; pass --replace or --uninstall')
    subprocess.run(['launchctl','bootout',domain,str(plist)],check=False);plist.unlink()
obj={'Label':label,'ProgramArguments':[str(binary),str(config)],'EnvironmentVariables':{'GRYVIA_AGENT_TOKEN':token},'RunAtLoad':True,'KeepAlive':True,'ThrottleInterval':10,'StandardOutPath':str(logs/'agent.log'),'StandardErrorPath':str(logs/'agent-error.log')}
fd=os.open(plist,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'wb') as f:plistlib.dump(obj,f)
import time
for attempt in range(5):
    r=subprocess.run(['launchctl','bootstrap',domain,str(plist)],capture_output=True,text=True)
    if r.returncode==0:break
    time.sleep(1+attempt)
else:raise SystemExit('launchctl bootstrap failed: '+r.stderr.strip())
print('Installed',label)
