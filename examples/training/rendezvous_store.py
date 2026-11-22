# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

"""A standalone c10d rendezvous store for elastic `torchrun`, so the job survives losing any worker, index 0 included.

torchrun's c10d rendezvous keeps its state in a TCPStore. With `--rdzv_endpoint=$(MASTER_ADDR):$(MASTER_PORT)` the
agent in pod 0 hosts that store, and losing pod 0 loses the rendezvous for everyone. Run this server as its own
Deployment and Service instead, and point every worker at it:

  torchrun --nnodes=$NNODES --nproc_per_node=$NPROC_PER_NODE --rdzv_backend=c10d \
    --rdzv_endpoint=<service>:29400 --rdzv_conf=is_host=0 --rdzv_id=$JOB ... elastic_train.py

`is_host=0` makes every agent a client of this store. The process group's master is chosen per rendezvous round
(the new rank 0 after a loss), not taken from MASTER_ADDR. The store is in memory: restarting this server loses
the rendezvous state of running jobs, so give it its own pod that the training nodes' failures do not take down.

The workers also need TORCH_DISABLE_SHARE_RDZV_TCP_STORE=1 (Gryvia sets it on elastic jobs): otherwise torch caches
the first round's rank 0 as the workers' store host, and the new rank 0 fails an assertion when it restarts them.

Environment: RDZV_PORT (29400).
"""
import os
import signal
import sys

import torch.distributed as dist


def main():
    port = int(os.environ.get('RDZV_PORT', '29400'))
    store = dist.TCPStore('0.0.0.0', port, is_master=True, wait_for_workers=False)
    print('rendezvous store listening on port %d' % port, flush=True)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    try:
        signal.pause()
    finally:
        del store


if __name__ == '__main__':
    main()
