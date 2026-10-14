# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import hashlib, json, tempfile, unittest

from server import ed25519
from server.core import Problem, Store
from tests import ed25519_sign as signer

SEED = bytes(range(1, 33))            # the same seed used to produce the CryptoKit vector below
CRYPTOKIT_PUB = "79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664"
CRYPTOKIT_SIG = "069281280c8ce1a7f308c27f57562a5ee8d1ec39b8483785401109a737e81634dcee439eff8e610b0dc452c9e7c67a932ec89b6eaed8a5e66c47c5103b99b406"


class Ed25519Tests(unittest.TestCase):
    def test_rfc8032_vectors(self):
        self.assertTrue(ed25519.verify_hex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", b"",
            "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"))
        self.assertTrue(ed25519.verify_hex("3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", b"\x72",
            "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"))

    def test_verifies_a_signature_made_by_cryptokit_on_a_mac(self):
        msg = b'{"job":"abc","v":1}'
        self.assertEqual(signer.public_key(SEED).hex(), CRYPTOKIT_PUB)
        self.assertTrue(ed25519.verify_hex(CRYPTOKIT_PUB, msg, CRYPTOKIT_SIG))
        # CryptoKit's signatures are randomised (hedged), so they are not byte-identical to the RFC 8032 deterministic
        # signature; both must verify.
        self.assertTrue(ed25519.verify(bytes.fromhex(CRYPTOKIT_PUB), msg, signer.sign(SEED, msg)))

    def test_rejects_tampering_and_garbage(self):
        msg = b'{"job":"abc","v":1}'
        self.assertFalse(ed25519.verify_hex(CRYPTOKIT_PUB, msg + b" ", CRYPTOKIT_SIG))
        bad = bytearray(bytes.fromhex(CRYPTOKIT_SIG)); bad[10] ^= 1
        self.assertFalse(ed25519.verify(bytes.fromhex(CRYPTOKIT_PUB), msg, bytes(bad)))
        for pk, sig in [("00" * 32, CRYPTOKIT_SIG), (CRYPTOKIT_PUB, "00" * 64), ("zz", CRYPTOKIT_SIG), (CRYPTOKIT_PUB, "ab")]:
            self.assertFalse(ed25519.verify_hex(pk, msg, sig))


class ProvenanceFlowTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.s = Store(self.tmp.name + "/db")
        self.pub = signer.public_key(SEED).hex()

    def tearDown(self):
        self.s.db.close(); self.tmp.cleanup()

    def beat(self, key=None):
        self.s.register("mac", dict(budgetGiB=8, runtimes=["download", "bench", "demo", "train", "serve", "fuse"], attestKey=key or self.pub))

    def run_job(self, spec, logs, seed=SEED, tamper=None, success=True):
        self.beat()
        jid = self.s.submit("default", dict(mode="shared", memoryGiB=1, args=[], **spec))
        c = self.s.claim("mac")
        self.assertEqual(c["id"], jid)
        result_line = next((l for l in logs if l.startswith('{"kind": "result"')), None)
        if logs:
            self.s.logs("mac", jid, c["lease"], logs)
        payload = dict(v=1, job=jid, kind=spec.get("kind", "inference"), modelId=spec.get("modelId", ""), specSha256=c["specSha256"],
                       resultSha256=hashlib.sha256(result_line.encode()).hexdigest() if result_line else "", exitCode=0, success=success)
        payload.update(tamper or {})
        text = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        att = {"payload": text, "signature": signer.sign(seed, text.encode()).hex()}
        self.s.complete("mac", jid, c["lease"], success, {}, att)
        return jid

    def model(self, mid="m"):
        return next(m for m in self.s.snapshot()["models"] if m["id"] == mid)

    def test_signed_download_gives_verified_model(self):
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        line = json.dumps(dict(kind="result", path="/models/m.gguf", sha256="ab" * 32, bytes=10))
        jid = self.run_job(dict(runtime="download", kind="download", modelId="m"), [line])
        prov = self.model()["provenance"]
        self.assertEqual((prov["verified"], prov["chainVerified"], prov["chain"][0]["kind"], prov["chain"][0]["signer"]), (True, True, "download", "mac"))
        att = self.s.attestations(jid)[0]
        self.assertTrue(att["verified"])

    def test_wrong_key_wrong_spec_and_wrong_result_are_rejected(self):
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        line = json.dumps(dict(kind="result", path="/m.gguf", sha256="cd" * 32, bytes=1))
        self.run_job(dict(runtime="download", kind="download", modelId="m"), [line], seed=bytes(range(2, 34)))        # other key
        self.assertFalse(self.model()["provenance"]["verified"])
        self.assertIn("signature does not verify", self.s.attestations()[0]["why"])
        self.s.model("m2", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        self.run_job(dict(runtime="download", kind="download", modelId="m2"), [line], tamper={"specSha256": "00" * 32})
        self.assertIn("spec hash", self.s.attestations()[0]["why"])
        self.s.model("m3", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        self.run_job(dict(runtime="download", kind="download", modelId="m3"), [line], tamper={"resultSha256": "11" * 32})
        self.assertIn("result line", self.s.attestations()[0]["why"])

    def test_key_is_pinned_on_first_use(self):
        self.beat()
        self.beat(key="ff" * 32)                         # a different key later must not replace the pinned one
        pinned = self.s.db.execute("SELECT pub FROM node_keys WHERE node='mac'").fetchone()["pub"]
        self.assertEqual(pinned, self.pub)
        kinds = [e["kind"] for e in self.s.snapshot()["events"]]
        self.assertIn("attest.key_mismatch", kinds)
        self.s.reset_key("mac")
        self.beat(key="ff" * 32)
        self.assertEqual(self.s.db.execute("SELECT pub FROM node_keys WHERE node='mac'").fetchone()["pub"], "ff" * 32)

    def test_unsigned_completion_leaves_model_unverified(self):
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        self.beat()
        jid = self.s.submit("default", dict(runtime="download", kind="download", modelId="m", mode="shared", memoryGiB=1, args=[]))
        c = self.s.claim("mac")
        self.s.logs("mac", jid, c["lease"], [json.dumps(dict(kind="result", path="/m.gguf", sha256="aa" * 32, bytes=1))])
        self.s.complete("mac", jid, c["lease"], True, {})
        self.assertFalse(self.model()["provenance"]["verified"])

    def test_adapter_chain_reports_unattested_base(self):
        self.s.model("base", dict(memoryGiB=1, runtime="mlx", status="ready", source="org/base"))
        line = json.dumps(dict(kind="result", adapterPath="/runs/a", finalValLoss=1.0))
        self.run_job(dict(runtime="train", kind="train", modelId="base", datasetId="d"), [line])
        child = next(m for m in self.s.snapshot()["models"] if m.get("parent") == "base")
        prov = child["provenance"]
        self.assertTrue(prov["verified"])
        self.assertFalse(prov["chainVerified"])        # the base model was never attested on this fleet
        self.assertTrue(prov["baseUnattested"])
        self.assertEqual(prov["chain"][0]["kind"], "train")

    def test_require_verified_policy_blocks_unverified_models_only(self):
        self.s.require_verified = True
        self.s.model("manual", dict(memoryGiB=1, runtime="llama", status="ready", path="/m.gguf"))
        with self.assertRaises(Problem) as e:
            self.s.submit("default", dict(runtime="serve", kind="serve", modelId="manual", memoryGiB=1, mode="shared", args=[]))
        self.assertEqual(e.exception.status, 403)
        self.s.model("m", dict(memoryGiB=1, runtime="llama", status="onboarding"))
        self.run_job(dict(runtime="download", kind="download", modelId="m"), [json.dumps(dict(kind="result", path="/m.gguf", sha256="ee" * 32, bytes=1))])
        self.s.submit("default", dict(runtime="serve", kind="serve", modelId="m", memoryGiB=1, mode="shared", args=[]))   # verified: allowed


if __name__ == "__main__":
    unittest.main()
