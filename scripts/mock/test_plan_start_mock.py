"""The open plan's current start presentation and its legacy/gated mock variants (#17853)."""
import unittest

from test_readiness_mock import hub


class PlanStartMockTests(unittest.TestCase):
    def test_open_plan_projection_and_compatibility_variants(self):
        cases = (
            ({}, {"reason": "start_authorization", "remember": [0]}),
            ({"MOCK_PLAN_START_REASON": "owner_step"}, {"reason": "owner_step"}),
            ({"MOCK_PLAN_START_REASON": "future_gate"}, {"reason": "future_gate"}),
        )
        for env, expected in cases:
            with self.subTest(env=env):
                h = hub(self, MOCK_TASKS="1", **env)
                status, body = h.call("GET", "/api/v1/task-plans/plan_mock_3")
                self.assertEqual(status, 200, body)
                self.assertEqual(body["plan"].get("start"), expected)

        h = hub(self, MOCK_TASKS="1", MOCK_PLAN_START_REASON="legacy")
        _, body = h.call("GET", "/api/v1/task-plans/plan_mock_3")
        self.assertNotIn("start", body["plan"])

    def test_settled_plan_omits_start_on_the_direct_read(self):
        h = hub(self, MOCK_TASKS="1")
        status, body = h.call("POST", "/api/v1/task-plans/plan_mock_3/start", {"remember": False})
        self.assertEqual(status, 200, body)
        self.assertNotIn("start", body["plan"])
        status, log = h.call("GET", "/mock/plan-starts")
        self.assertEqual(status, 200, log)
        self.assertEqual(log["requests"], [{"remember": False}])
        _, body = h.call("GET", "/api/v1/task-plans/plan_mock_3")
        self.assertEqual(body["plan"]["state"], "started")
        self.assertNotIn("start", body["plan"])

    def test_start_request_log_preserves_the_true_choice(self):
        h = hub(self, MOCK_TASKS="1")
        status, body = h.call("POST", "/api/v1/task-plans/plan_mock_3/start", {"remember": True})
        self.assertEqual(status, 200, body)
        status, log = h.call("GET", "/mock/plan-starts")
        self.assertEqual(status, 200, log)
        self.assertEqual(log["requests"], [{"remember": True}])


if __name__ == "__main__":
    unittest.main()
