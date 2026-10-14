#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Core ML adapter for numeric JSON inputs. Accelerator choice remains Core ML's."""
import argparse,json,time

def main():
    p=argparse.ArgumentParser();p.add_argument('--model',required=True);p.add_argument('--inputs',required=True);a=p.parse_args()
    import coremltools as ct
    import numpy as np
    inputs=json.loads(a.inputs)
    if not isinstance(inputs,dict):p.error('inputs must be an object')
    model=ct.models.MLModel(a.model,compute_units=ct.ComputeUnit.ALL)
    start=time.monotonic();result=model.predict({k:np.asarray(v) for k,v in inputs.items()})
    print(json.dumps({'kind':'prediction','durationSeconds':time.monotonic()-start,'outputs':{k:v.tolist() if hasattr(v,'tolist') else v for k,v in result.items()}}),flush=True)
if __name__=='__main__':main()
