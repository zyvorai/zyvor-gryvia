# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
"""Test-only Ed25519 *signing* (RFC 8032) so provenance can be exercised without CryptoKit. Never shipped."""
import hashlib

from server import ed25519 as e


def _clamp(h):
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    return a | (1 << 254)


def _compress(pt):
    zinv = e._inv(pt[2])
    x, y = pt[0] * zinv % e._P, pt[1] * zinv % e._P
    return int.to_bytes(y | ((x & 1) << 255), 32, "little")


def public_key(seed: bytes) -> bytes:
    return _compress(e._mul(_clamp(hashlib.sha512(seed).digest()), e._G))


def sign(seed: bytes, message: bytes) -> bytes:
    h = hashlib.sha512(seed).digest()
    a, prefix, pub = _clamp(h), h[32:], public_key(seed)
    r = int.from_bytes(hashlib.sha512(prefix + message).digest(), "little") % e._Q
    R = _compress(e._mul(r, e._G))
    k = int.from_bytes(hashlib.sha512(R + pub + message).digest(), "little") % e._Q
    return R + int.to_bytes((r + k * a) % e._Q, 32, "little")
