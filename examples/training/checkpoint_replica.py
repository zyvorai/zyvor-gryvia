"""Copy committed checkpoint steps to a second location, and restore from it when the local root is empty.

A GryviaCheckpointGuard with checkpointPolicy.replication.target passes it as GRYVIA_CHECKPOINT_REPLICA:

    file:///mnt/replica/my-job      a directory on another mounted volume
    s3://bucket/prefix              object storage through boto3 (install it; credentials from the usual
                                    AWS environment, AWS_ENDPOINT_URL for S3-compatible stores)

replicate(root, target) copies the latest committed step of coordinated_checkpoint/dcp_checkpoint (steps/<S>/
including its COMMIT record) and only then moves the replica's COMMITTED pointer, so a reader never sees a step
that is not complete; it keeps the newest `keep` steps. restore(target, root) copies the replica's committed step
into an empty root, so a job that lost its volume (or runs in another cluster) resumes from the replica. Call
both from rank 0 only.
"""
import json
import os
from pathlib import Path
import shutil
import tempfile
from urllib.parse import urlparse

import coordinated_checkpoint as cc


def _split(target):
    u = urlparse(target)
    if u.scheme == 'file' and u.path.startswith('/') and not u.netloc:
        return 'file', Path(u.path), None
    if u.scheme == 's3' and u.netloc:
        return 's3', u.netloc, u.path.strip('/')
    raise ValueError('replica target must be file:///<path> or s3://<bucket>/<prefix>')


def _files(sdir):
    for p in sorted(sdir.rglob('*')):
        if p.is_symlink():
            raise ValueError('refusing to replicate symlink %s' % p)
        if p.is_file():
            yield p


def _s3(client):
    if client is not None:
        return client
    import boto3  # optional dependency, only for s3:// targets
    return boto3.client('s3', endpoint_url=os.environ.get('AWS_ENDPOINT_URL') or None)


def _key(prefix, *parts):
    return '/'.join([p for p in [prefix] + list(parts) if p])


def replicate(root, target, keep=2, s3_client=None):
    """Copy the latest committed step to target. Returns the step, or None when nothing is committed."""
    step = cc.latest_committed_step(root)
    if step is None:
        return None
    sdir = Path(root) / cc.STEPS / str(step)
    if not (sdir / 'COMMIT').exists():
        raise ValueError('COMMITTED names step %d but it has no COMMIT record' % step)
    kind, base, prefix = _split(target)
    if kind == 'file':
        steps = base / cc.STEPS
        steps.mkdir(parents=True, exist_ok=True)
        dest = steps / str(step)
        if not (dest / 'COMMIT').exists():
            tmp = Path(tempfile.mkdtemp(prefix='.%d-' % step, dir=steps))
            try:
                for f in _files(sdir):
                    out = tmp / f.relative_to(sdir)
                    out.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(f, out)
                    with open(out, 'rb') as fh:
                        os.fsync(fh.fileno())
                shutil.rmtree(dest, ignore_errors=True)
                os.replace(tmp, dest)
            finally:
                shutil.rmtree(tmp, ignore_errors=True)
        current = cc.latest_committed_step(base)
        if current is None or step > current:
            cc._atomic_write(base / cc.COMMITTED, json.dumps({'step': step}).encode())
        cc.prune(base, keep=keep)
        return step
    s3 = _s3(s3_client)
    for f in _files(sdir):
        s3.upload_file(str(f), base, _key(prefix, cc.STEPS, str(step), f.relative_to(sdir).as_posix()))
    s3.put_object(Bucket=base, Key=_key(prefix, cc.COMMITTED), Body=json.dumps({'step': step}).encode())
    return step


def restore(target, root, s3_client=None):
    """Copy the replica's committed step into root when root has no committed step. Returns the step or None."""
    if cc.latest_committed_step(root) is not None:
        return None
    kind, base, prefix = _split(target)
    root = Path(root)
    if kind == 'file':
        step = cc.latest_committed_step(base)
        if step is None:
            return None
        src = base / cc.STEPS / str(step)
        files = list(_files(src))
        rel = [f.relative_to(src).as_posix() for f in files]

        def fetch(name, out):
            shutil.copyfile(src / name, out)
    else:
        s3 = _s3(s3_client)
        try:
            body = s3.get_object(Bucket=base, Key=_key(prefix, cc.COMMITTED))['Body'].read()
        except Exception as e:  # botocore raises NoSuchKey through a client-specific class
            if 'NoSuchKey' in type(e).__name__ or 'NoSuchKey' in str(e):
                return None
            raise
        step = json.loads(body).get('step')
        if not isinstance(step, int) or isinstance(step, bool) or step < 0:
            raise ValueError('replica COMMITTED is invalid')
        step_prefix = _key(prefix, cc.STEPS, str(step)) + '/'
        rel = []
        for page in s3.get_paginator('list_objects_v2').paginate(Bucket=base, Prefix=step_prefix):
            rel += [o['Key'][len(step_prefix):] for o in page.get('Contents', [])]

        def fetch(name, out):
            s3.download_file(base, step_prefix + name, str(out))
    if 'COMMIT' not in rel:
        raise ValueError('replica step %d has no COMMIT record' % step)
    for name in rel:
        if name.startswith('/') or '..' in Path(name).parts:
            raise ValueError('unsafe file name %r in replica' % name)
    dest = root / cc.STEPS / str(step)
    shutil.rmtree(dest, ignore_errors=True)
    for name in rel:
        out = dest / name
        out.parent.mkdir(parents=True, exist_ok=True)
        fetch(name, out)
    cc._atomic_write(root / cc.COMMITTED, json.dumps({'step': step}).encode())
    return step
