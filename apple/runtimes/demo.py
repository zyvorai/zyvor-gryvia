#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Deterministic CPU-only smoke worker; never presented as a GPU benchmark."""
import argparse,json,time,os
p=argparse.ArgumentParser();p.add_argument('--seconds',type=int,default=3);a=p.parse_args()
if not 0<=a.seconds<=60:p.error('seconds must be 0..60')
for i in range(a.seconds):print(json.dumps({'kind':'progress','step':i+1,'job':os.getenv('GRYVIA_JOB_ID')}),flush=True);time.sleep(1)
print(json.dumps({'kind':'complete','hardware':'cpu-demo','gpuBenchmark':False}),flush=True)
