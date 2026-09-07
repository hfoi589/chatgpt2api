from __future__ import annotations

import unittest
from datetime import datetime
from concurrent.futures import ThreadPoolExecutor
from threading import Lock
from zoneinfo import ZoneInfo

from services.quota_service import QuotaExceededError, QuotaService


class FakeAuthService:
    def __init__(self, item):
        self.item = item
        self.saved = 0
        self.lock = Lock()

    def get_key_for_usage(self, key_id):
        with self.lock:
            return dict(self.item) if self.item["id"] == key_id else None

    def save_usage_item(self, item):
        with self.lock:
            self.item = dict(item)
            self.saved += 1

    def mutate_usage_item(self, key_id, mutator):
        with self.lock:
            if self.item["id"] != key_id:
                return None
            updated = mutator(dict(self.item))
            self.item = dict(updated)
            self.saved += 1
            return dict(updated)


class QuotaServiceTests(unittest.TestCase):
    def make(self, **overrides):
        item = {
            "id": "key-1",
            "role": "user",
            "enabled": True,
            "daily_quota": None,
            "weekly_quota": None,
            "total_quota": 3,
            "daily_used": 0,
            "weekly_used": 0,
            "total_used": 0,
            "lifetime_used": 0,
            "daily_window_start": None,
            "weekly_window_start": None,
            "route_account_ids": [],
            **overrides,
        }
        auth = FakeAuthService(item)
        return QuotaService(auth, now_provider=lambda: datetime(2026, 9, 7, 12, tzinfo=ZoneInfo("America/New_York"))), auth

    def test_total_quota_reserves_and_rejects_when_exhausted(self):
        service, auth = self.make(total_quota=2)
        reservation = service.reserve_images({"id": "key-1", "role": "user"}, 2)
        self.assertEqual(reservation.amount, 2)
        service.settle_images(reservation, 1)
        self.assertEqual(auth.item["total_quota"], 1)
        self.assertEqual(auth.item["total_used"], 1)
        self.assertEqual(auth.item["lifetime_used"], 1)
        with self.assertRaises(QuotaExceededError):
            service.reserve_images({"id": "key-1", "role": "user"}, 2)

    def test_total_balance_can_be_replenished_after_exhaustion(self):
        service, auth = self.make(total_quota=1)
        service.reserve_images({"id": "key-1", "role": "user"}, 1)
        self.assertEqual(auth.item["total_quota"], 0)
        auth.item["total_quota"] = 2
        reservation = service.reserve_images({"id": "key-1", "role": "user"}, 2)
        self.assertEqual(reservation.amount, 2)
        self.assertEqual(auth.item["total_quota"], 0)

    def test_daily_and_weekly_windows_reset_in_new_york_time(self):
        service, auth = self.make(daily_quota=2, weekly_quota=3, total_quota=None)
        service.reserve_images({"id": "key-1", "role": "user"}, 2)
        self.assertEqual(auth.item["daily_used"], 2)
        auth.item["daily_window_start"] = "2026-09-06"
        auth.item["weekly_window_start"] = "2026-08-31"
        service.reserve_images({"id": "key-1", "role": "user"}, 1)
        self.assertEqual(auth.item["daily_used"], 1)
        self.assertEqual(auth.item["weekly_used"], 1)

    def test_public_usage_reports_remaining_and_route(self):
        service, _ = self.make(daily_quota=10, daily_used=4, total_quota=20, total_used=7, lifetime_used=7,
                               daily_window_start="2026-09-07", weekly_window_start="2026-09-07",
                               route_account_ids=["pool-1"])
        usage = service.public_usage({"id": "key-1", "role": "user"})
        self.assertEqual(usage["lifetime_used"], 7)
        self.assertEqual(usage["daily"]["remaining"], 6)
        self.assertEqual(usage["total"]["remaining"], 20)
        self.assertFalse(usage["blocked"])

    def test_concurrent_reservations_cannot_oversell(self):
        service, auth = self.make(total_quota=1)

        def reserve():
            try:
                service.reserve_images({"id": "key-1", "role": "user"}, 1)
                return True
            except QuotaExceededError:
                return False

        with ThreadPoolExecutor(max_workers=8) as executor:
            results = list(executor.map(lambda _index: reserve(), range(8)))
        self.assertEqual(sum(results), 1)
        self.assertEqual(auth.item["total_used"], 1)


if __name__ == "__main__":
    unittest.main()
