from __future__ import annotations

import unittest
from datetime import datetime
from zoneinfo import ZoneInfo

from services.quota_migration import migrate_usage_from_logs


class FakeAuth:
    def __init__(self):
        self.items = [{
            "id": "key-1", "daily_used": 0, "weekly_used": 0,
            "total_used": 0, "lifetime_used": 0,
            "total_quota": 5,
            "daily_window_start": None, "weekly_window_start": None,
            "usage_migration_version": 0,
        }]

    def list_key_snapshots(self):
        return [dict(item) for item in self.items]

    def mutate_usage_item(self, key_id, mutator):
        item = next(item for item in self.items if item["id"] == key_id)
        result = mutator(item)
        return result


class FakeLogs:
    def list(self, **_kwargs):
        return [
            {"type": "call", "detail": {"key_id": "key-1", "endpoint": "/v1/images/generations", "status": "success", "urls": ["a", "b"], "ended_at": "2026-09-07 10:00:00"}},
            {"type": "call", "detail": {"key_id": "key-1", "endpoint": "/v1/images/generations", "status": "failed", "ended_at": "2026-09-07 11:00:00"}},
        ]


class QuotaMigrationTests(unittest.TestCase):
    def test_backfills_successful_image_count_once(self):
        auth = FakeAuth()
        result = migrate_usage_from_logs(
            auth,
            FakeLogs(),
            now_provider=lambda: datetime(2026, 9, 7, 12, tzinfo=ZoneInfo("America/New_York")),
        )
        self.assertEqual(result["updated"], 1)
        self.assertEqual(auth.items[0]["lifetime_used"], 2)
        self.assertEqual(auth.items[0]["total_quota"], 3)
        self.assertEqual(auth.items[0]["daily_used"], 2)
        second = migrate_usage_from_logs(auth, FakeLogs())
        self.assertEqual(second["updated"], 0)
        self.assertEqual(auth.items[0]["lifetime_used"], 2)


if __name__ == "__main__":
    unittest.main()
