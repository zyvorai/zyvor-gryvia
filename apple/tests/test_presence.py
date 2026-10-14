# SPDX-License-Identifier: BUSL-1.1
# Copyright © 2026 Zyvor AI Labs Private Limited
import tempfile, unittest
from server.core import DEFAULT_POLICY, Problem, Store, clean_policy, policy_gate


def presence(**kw):
    base = dict(idleSeconds=1000, locked=False, displaySleep=False, onBattery=False, batteryPct=100, lowPowerMode=False, micInUse=False)
    base.update(kw)
    return base


class GateTests(unittest.TestCase):
    def test_always_allows_unless_power_rules(self):
        self.assertEqual(policy_gate(DEFAULT_POLICY, presence(idleSeconds=0)), (True, ""))
        self.assertEqual(policy_gate(DEFAULT_POLICY, presence(lowPowerMode=True))[0], False)
        self.assertEqual(policy_gate(DEFAULT_POLICY, presence(onBattery=True, batteryPct=10))[1], "battery below 30%")
        pol = clean_policy({"allowOnBattery": False})
        self.assertEqual(policy_gate(pol, presence(onBattery=True))[1], "on battery")

    def test_idle_mode_waits_for_owner(self):
        pol = clean_policy({"mode": "idle", "idleMinutes": 5})
        self.assertEqual(policy_gate(pol, presence(idleSeconds=30)), (False, "owner is active"))
        self.assertTrue(policy_gate(pol, presence(idleSeconds=301))[0])
        self.assertTrue(policy_gate(pol, presence(idleSeconds=5, locked=True))[0])        # locked screen counts as away
        self.assertEqual(policy_gate(pol, presence(idleSeconds=999, micInUse=True))[1], "microphone in use (call?)")

    def test_paused_always_blocks_and_missing_presence_never_blocks(self):
        self.assertFalse(policy_gate(clean_policy({"mode": "paused"}), presence())[0])
        self.assertTrue(policy_gate(clean_policy({"mode": "idle"}), None)[0])

    def test_policy_validation_rejects_typos_and_bad_values(self):
        for bad in ({"mod": "idle"}, {"mode": "sometimes"}, {"idleMinutes": -1}, {"minBatteryPct": 101}, {"allowOnBattery": "yes"}):
            with self.subTest(bad=bad), self.assertRaises(Problem):
                clean_policy(bad)


class SchedulerYieldTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.t = [1000.0]
        self.s = Store(self.tmp.name + "/db", clock=lambda: self.t[0])

    def tearDown(self):
        self.s.db.close(); self.tmp.cleanup()

    def beat(self, **pr):
        self.s.register("mac", dict(budgetGiB=8, runtimes=["demo"], presence=presence(**pr)))

    def job(self, **kw):
        spec = dict(runtime="demo", memoryGiB=1, mode="shared", args=[], retries=0)
        spec.update(kw)
        return self.s.submit("default", spec)

    def test_claim_blocked_while_owner_active_then_allowed(self):
        self.beat(idleSeconds=2)
        self.s.set_policy("mac", {"mode": "idle", "idleMinutes": 5})
        self.job()
        self.assertIsNone(self.s.claim("mac"))
        node = self.s.snapshot()["nodes"][0]
        self.assertEqual((node["gate"]["allowed"], node["gate"]["reason"], node["policy"]["mode"]), (False, "owner is active", "idle"))
        self.t[0] += 2; self.beat(idleSeconds=600)
        self.assertIsNotNone(self.s.claim("mac"))

    def test_running_job_yields_and_requeues_without_using_a_retry(self):
        self.beat(idleSeconds=900)
        self.s.set_policy("mac", {"mode": "idle", "idleMinutes": 5})
        j = self.job(retries=0)
        c = self.s.claim("mac")
        self.assertEqual(c["id"], j)
        self.t[0] += 2; self.beat(idleSeconds=1)                       # the owner touches the keyboard
        st = next(x for x in self.s.snapshot()["jobs"] if x["id"] == j)
        self.assertEqual((st["state"], st["reason"]), ("cancelling", "owner returned"))
        r = self.s.renew("mac", j, c["lease"])
        self.assertTrue(r["cancel"])
        self.s.complete("mac", j, c["lease"], False, {})                # agent stopped the process
        st = next(x for x in self.s.snapshot()["jobs"] if x["id"] == j)
        self.assertEqual((st["state"], st["attempt"]), ("queued", 0))   # back in the queue, attempt not consumed
        self.assertIsNone(self.s.claim("mac"))                          # still active: waits
        self.t[0] += 2; self.beat(idleSeconds=900)
        again = self.s.claim("mac")
        self.assertEqual(again["id"], j)

    def test_pause_policy_stops_new_work_and_resume_restores_it(self):
        self.beat()
        self.job()
        self.s.set_policy("mac", {"mode": "paused"})
        self.assertIsNone(self.s.claim("mac"))
        self.s.set_policy("mac", {"mode": "always"})
        self.assertIsNotNone(self.s.claim("mac"))

    def test_policy_requires_known_node(self):
        with self.assertRaises(Problem):
            self.s.set_policy("ghost", {"mode": "paused"})


if __name__ == "__main__":
    unittest.main()


class EnergyTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.t = [1000.0]
        self.s = Store(self.tmp.name + "/db", clock=lambda: self.t[0])

    def tearDown(self):
        self.s.db.close(); self.tmp.cleanup()

    def beat(self, watts):
        self.s.register("mac", dict(budgetGiB=8, runtimes=["demo"], gpuUtilPct=10.0, power=dict(packageW=watts)))

    def test_energy_integrates_power_and_subtracts_idle_baseline(self):
        for _ in range(5):                              # idle: 6 W
            self.t[0] += 2; self.beat(6.0)
        j = self.s.submit("default", dict(runtime="demo", memoryGiB=1, mode="shared", args=[], retries=0))
        c = self.s.claim("mac")
        for _ in range(5):                              # busy: 26 W for 10 s -> 260 J, 200 J above the 6 W idle
            self.t[0] += 2; self.beat(26.0)
        self.s.complete("mac", j, c["lease"], True, {})
        en = self.s.results(kind="energy")[0]["body"]
        self.assertTrue(en["estimate"])
        self.assertAlmostEqual(en["idleWatts"], 6.0)
        self.assertTrue(180 <= en["joulesAboveIdle"] <= 260, en)
        usage = self.s.snapshot()["usage"][0]
        self.assertGreater(usage["kWh"], 0)

    def test_no_power_samples_means_no_energy_claim(self):
        self.s.register("mac", dict(budgetGiB=8, runtimes=["demo"], gpuUtilPct=10.0))
        j = self.s.submit("default", dict(runtime="demo", memoryGiB=1, mode="shared", args=[], retries=0))
        c = self.s.claim("mac")
        self.t[0] += 5
        self.s.complete("mac", j, c["lease"], True, {})
        self.assertEqual(self.s.results(kind="energy"), [])
