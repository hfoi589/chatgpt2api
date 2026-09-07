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
            "total_quota": kwargs["total_quota"], "route_account_ids": kwargs["route_account_ids"],
        }
        self.items = [item]
        return item, "sk-secret"


class FakeAccounts:
    def list_image_route_accounts(self):
        return [{"pool_account_id": "pool-1", "email": "one@example.test", "status": "正常", "quota": 3}]


class FakeQuota:
    def public_usage(self, identity):
        return {"lifetime_used": 2, "daily": {}, "weekly": {}, "total": {}, "blocked": False, "reason": None, "route_account_ids": []}


class UserQuotaApiTests(unittest.TestCase):
    def test_create_user_key_accepts_limits_and_route(self):
        auth = FakeAuth()
        app = FastAPI()
        with patch.object(accounts_module, "auth_service", auth), \
             patch.object(accounts_module, "account_service", FakeAccounts()), \
             patch.object(accounts_module, "quota_service", FakeQuota()), \
             patch.object(accounts_module, "require_admin", return_value={"role": "admin"}):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).post(
                "/api/auth/users",
                json={"name": "Alice", "daily_quota": 10, "weekly_quota": None, "total_quota": 5, "route_account_ids": ["pool-1"]},
            )
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(auth.items[0]["route_account_ids"], ["pool-1"])
        self.assertEqual(auth.items[0]["total_quota"], 5)

    def test_current_usage_endpoint_is_available_to_authenticated_user(self):
        app = FastAPI()
        with patch.object(accounts_module, "require_identity", return_value={"id": "key-1", "role": "user"}), \
             patch.object(accounts_module, "quota_service", FakeQuota()):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).get("/api/auth/me/usage", headers={"Authorization": "Bearer user"})
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(response.json()["lifetime_used"], 2)

    def test_route_catalog_does_not_expose_access_tokens(self):
        app = FastAPI()
        fake = FakeAccounts()
        with patch.object(accounts_module, "account_service", fake), \
             patch.object(accounts_module, "require_admin", return_value={"role": "admin"}):
            app.include_router(accounts_module.create_router())
            response = TestClient(app).get("/api/accounts/routes")
        self.assertEqual(response.status_code, 200)
        self.assertNotIn("access_token", response.json()["items"][0])


if __name__ == "__main__":
    unittest.main()
