"""Immutable SHA256 cache for mounted datasets/model files, suitable for an init container.

Requires a filesystem with working POSIX flock, rename and fsync. Every hit is reverified;
no mutable 'latest' alias or filename-based reuse. This copies files, not directory trees,
and does not claim to implement a distributed cache or GPUDirect Storage.
"""
import fcntl
import hashlib
import os
from pathlib import Path
import re
import stat
import tempfile


def _digest(path, max_bytes):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    count, digest = 0, hashlib.sha256()
    with os.fdopen(fd, 'rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError('artifact must be a regular file')
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            count += len(chunk)
            if count > max_bytes:
                raise ValueError('artifact exceeds size limit')
            digest.update(chunk)
    return digest.hexdigest(), count


def materialize(source, cache, expected_sha256, max_bytes=1024 ** 4):
    """Verify and atomically publish a mounted file. Return path, bytes and cacheHit.

    A digest mismatch never becomes visible as a valid cache artifact. Readers should
    treat returned files as immutable; authorization and filesystem permissions belong
    to the caller. The cache must be private to the intended tenant/trust domain.
    """
    if not re.fullmatch(r'[a-f0-9]{64}', expected_sha256):
        raise ValueError('expected SHA256 must contain 64 lowercase hex characters')
    if isinstance(max_bytes, bool) or not isinstance(max_bytes, int) or max_bytes <= 0:
        raise ValueError('invalid size limit')
    directory = Path(cache) / 'sha256'
    directory.mkdir(parents=True, exist_ok=True)
    if directory.is_symlink():
        raise ValueError('cache directory cannot be a symlink')
    output = directory / expected_sha256
    lockfd = os.open(directory / (expected_sha256 + '.lock'), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lockfd, 'a+b') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if output.is_symlink():
            raise ValueError('cache artifact cannot be a symlink')
        if output.exists():
            got, count = _digest(output, max_bytes)
            if got == expected_sha256:
                return {'path': str(output), 'bytes': count, 'cacheHit': True, 'sha256': got}
        tmpfd, tmpname = tempfile.mkstemp(prefix='.artifact-', dir=directory)
        temp = Path(tmpname)
        sourcefd = None
        try:
            sourcefd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
            if not stat.S_ISREG(os.fstat(sourcefd).st_mode):
                raise ValueError('source must be a regular file')
            count, digest = 0, hashlib.sha256()
            with os.fdopen(sourcefd, 'rb') as src, os.fdopen(tmpfd, 'wb') as dst:
                sourcefd, tmpfd = None, None
                for chunk in iter(lambda: src.read(1024 * 1024), b''):
                    count += len(chunk)
                    if count > max_bytes:
                        raise ValueError('artifact exceeds size limit')
                    digest.update(chunk)
                    dst.write(chunk)
                if digest.hexdigest() != expected_sha256:
                    raise ValueError('artifact checksum mismatch')
                dst.flush()
                os.fsync(dst.fileno())
            os.chmod(temp, 0o400)
            os.replace(temp, output)
            dirfd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(dirfd)
            finally:
                os.close(dirfd)
            return {'path': str(output), 'bytes': count, 'cacheHit': False, 'sha256': expected_sha256}
        finally:
            if sourcefd is not None:
                os.close(sourcefd)
            if tmpfd is not None:
                os.close(tmpfd)
            temp.unlink(missing_ok=True)
