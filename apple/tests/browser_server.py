# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
from server.app import make_server
from server.core import Store
import os
s=Store(':memory:');s.tenant('research',16,2)
s.register('mini-01',dict(budgetGiB=12,runtimes=['demo','mlx'],gpu='Browser test fixture · Apple GPU',thermal='nominal',gpuModel='Apple M6',gpuCores=12,gpuUtilPct=42.0,gpuInUseMiB=3072,totalGiB=36))
s.register('studio-01',dict(budgetGiB=48,runtimes=['mlx'],gpu='Browser test fixture · Apple GPU',thermal='nominal',gpuModel='Apple M3 Ultra',gpuCores=80,gpuUtilPct=5.0,gpuInUseMiB=900,totalGiB=96))
s.model('MLX local model',dict(memoryGiB=6,runtime='mlx',source='Local pinned revision'))
s.model('tiny-q4',dict(memoryGiB=1.5,runtime='llama',status='ready',path='/models/tiny-q4.gguf',node='mini-01',quant='Q4_K_M',sizeBytes=700000000,source='owner/tiny/tiny-q4.gguf'))
s.dataset('toy',dict(path='/models/_datasets/toy.jsonl',rows=40,bytes=2048,status='ready',node='mini-01',source='local'))
s.submit('default',dict(runtime='demo',model='Browser fixture job',memoryGiB=4));s.claim('mini-01')
s.submit('research',dict(runtime='mlx',model='Evaluation fixture',memoryGiB=6))
make_server(s,{'browser-test-admin-token-00000000':{'role':'admin'}},port=int(os.getenv('TEST_PORT','8765'))).serve_forever()
