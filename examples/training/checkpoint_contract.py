"""Verify the existing all-ranks checkpoint against immutable model/dataset identity.

This checks bytes and identity without deserializing framework payloads. It never asserts
that sharded optimizer state can be resharded. A trainer binds this contract once before
calling coordinated_checkpoint.save_global; consumers verify it before recovery.
"""
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import tempfile

from coordinated_checkpoint import latest_committed_step


def _identity(model_digest, dataset_digest, framework):
    if any(not re.fullmatch(r'[a-f0-9]{64}', d) for d in (model_digest, dataset_digest)):
        raise ValueError('model and dataset must have SHA256 identities')
    if framework not in ('pytorch', 'generic'):
        raise ValueError('unsupported framework')
    return {'version': 1, 'modelDigest': model_digest, 'datasetDigest': dataset_digest, 'framework': framework}


def _read(path, max_bytes):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError('checkpoint metadata must be a regular file')
        data = stream.read(max_bytes + 1)
        if len(data) > max_bytes:
            raise ValueError('checkpoint metadata exceeds limit')
        return json.loads(data)


def bind_contract(root, model_digest, dataset_digest, framework='pytorch'):
    identity = _identity(model_digest, dataset_digest, framework)
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    contract = root / 'CONTRACT.json'
    lockfd = os.open(root / '.contract.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lockfd, 'a+b') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if contract.exists() or contract.is_symlink():
            if _read(contract, 16384) != identity:
                raise ValueError('checkpoint root belongs to a different model/dataset contract')
            return identity
        if latest_committed_step(root) is not None:
            raise ValueError('cannot bind identity retroactively to an existing checkpoint')
        fd, name = tempfile.mkstemp(prefix='.contract-', dir=root)
        try:
            with os.fdopen(fd, 'w') as stream:
                json.dump(identity, stream)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(name, contract)
            dirfd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(dirfd)
            finally:
                os.close(dirfd)
        finally:
            Path(name).unlink(missing_ok=True)
    return identity


def verify_committed(root, model_digest, dataset_digest, framework='pytorch', max_rank_bytes=1024 ** 4):
    """Stream-verify EVERY rank. Returns recovery metadata; durableStorage stays unqualified.

    The caller must separately qualify storage durability before setting durableStorage=true.
    Interrupted/uncommitted steps are ignored by the existing COMMITTED pointer protocol.
    """
    root = Path(root)
    identity = _identity(model_digest, dataset_digest, framework)
    if root.is_symlink() or (root / 'steps').is_symlink():
        raise ValueError('checkpoint root and steps directory cannot be symlinks')
    if _read(root / 'CONTRACT.json', 16384) != identity:
        raise ValueError('model/dataset checkpoint contract mismatch')
    pointer = _read(root / 'COMMITTED', 16384)
    step = pointer.get('step')
    if isinstance(step, bool) or not isinstance(step, int) or step < 0:
        raise ValueError('invalid committed step')
    directory = root / 'steps' / str(step)
    if directory.is_symlink():
        raise ValueError('checkpoint step cannot be a symlink')
    path = directory / 'COMMIT'
    record = _read(path, 8 * 1024 * 1024)
    world = record.get('world_size')
    if (isinstance(world, bool) or not isinstance(world, int) or not 1 <= world <= 65536
            or record.get('version') != 1 or type(record.get('step')) is not int or record.get('step') != step):
        raise ValueError('invalid checkpoint commit')
    ranks = record.get('ranks')
    if not isinstance(ranks, list) or len(ranks) != world:
        raise ValueError('incomplete rank commit')
    if any(not isinstance(r, dict) or type(r.get('rank')) is not int for r in ranks):
        raise ValueError('invalid rank manifest')
    if {r['rank'] for r in ranks} != set(range(world)):
        raise ValueError('missing or duplicate rank')
    if type(max_rank_bytes) is not int or max_rank_bytes <= 0:
        raise ValueError('invalid checkpoint size limit')
    for r in ranks:
        if type(r.get('bytes')) is not int or r['bytes'] < 0:
            raise ValueError('invalid rank payload size')
        fd = os.open(directory / ('rank-%d.bin' % r['rank']), os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                raise ValueError('rank payload must be a regular file')
            size, digest = 0, hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                size += len(chunk)
                if size > max_rank_bytes:
                    raise ValueError('rank payload exceeds size limit')
                digest.update(chunk)
            if size != r.get('bytes') or digest.hexdigest() != r.get('sha256'):
                raise ValueError('checkpoint rank checksum mismatch')
    return {'step': step, 'modelDigest': model_digest, 'datasetDigest': dataset_digest,
            'framework': framework, 'worldSize': world, 'ranks': list(range(world)),
            'integrityVerified': True, 'globallyCommitted': True, 'durableStorage': False,
            'optimizerReshardable': False,
            'committedAt': datetime.fromtimestamp(path.stat().st_mtime, timezone.utc).isoformat()}
