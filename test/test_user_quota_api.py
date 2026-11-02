from __future__ import annotations

import unittest
from unittest.mock import patch

from fastapi import FastAPI
from fastapi.testclient import TestClient

import api.accounts as accounts_module


class FakeAuth:
    def __init__(self):
        self.items = []

    def list_keys(self, role=None):
        return self.items

    def create_key(self, **kwargs):
        item = {
            "id": "key-1", "name": kwargs["name"], "role": "user", "enabled": True,
            "daily_quota": kwargs["daily_quota"], "weekly_quota": kwargs["weekly_quota"],
            "total_quota": kwargs["total_quota"],
        }
        self.items = [item]
        return item, "sk-secret"

    def update_key(self, key_id, updates, *, role=None):
        item = next((item for item in self.items if item["id"] == key_id), None)
        if item is None or (role is not None and item.get("role") != role):
            return None
        item.update(updates)
        item.pop("route_account_ids", None)
        return item


class FakeQuota:
    def public_usage(self, identity):
        return {"lifetime_used": 2, "daily": {}, "weekly": {}, "total": {}, "blocked": False, "reason": None}


class UserQuotaApiTests(unittest.TestCase):
    def test_create_user_key_accepts_quota_limits(self):
        auth = FakeAuth()
        app = FastAPI()
        with patch.object(accounts_module, "auth_service", auth), \
             patch.object(accounts_module, "quota_service", FakeQuota()), \
             patch.object(accounts_module, "require_admin", return_value={"role": "admin"}):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).post(
                "/api/auth/users",
                json={"name": "Alice", "daily_quota": 10, "weekly_quota": None, "total_quota": 5},
            )
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(auth.items[0]["total_quota"], 5)

    def test_stale_legacy_route_id_does_not_block_key_update(self):
        stale_route_id = "9da865ad-bdac-452e-b616-1c8d2f473282"
        auth = FakeAuth()
        auth.items = [{
            "id": "key-1", "name": "Alice", "role": "user", "enabled": True,
            "daily_quota": None, "weekly_quota": None, "total_quota": None,
            "route_account_ids": [stale_route_id],
        }]
        app = FastAPI()
        with patch.object(accounts_module, "auth_service", auth), \
             patch.object(accounts_module, "quota_service", FakeQuota()), \
             patch.object(accounts_module, "require_admin", return_value={"role": "admin"}):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).post(
                "/api/auth/users/key-1",
                json={"daily_quota": 10, "route_account_ids": [stale_route_id]},
            )
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(response.json()["item"]["daily_quota"], 10)
        self.assertNotIn("route_account_ids", response.json()["item"])

    def test_current_usage_endpoint_is_available_to_authenticated_user(self):
        app = FastAPI()
        with patch.object(accounts_module, "require_identity", return_value={"id": "key-1", "role": "user"}), \
             patch.object(accounts_module, "quota_service", FakeQuota()):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).get("/api/auth/me/usage", headers={"Authorization": "Bearer user"})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(response.json()["lifetime_used"], 2)

if __name__ == "__main__":
    unittest.main()
