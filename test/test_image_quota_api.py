from __future__ import annotations

import unittest
from unittest.mock import patch

from fastapi import FastAPI
from fastapi.testclient import TestClient

import api.ai as ai_module
from services.quota_service import QuotaExceededError


class ImageQuotaApiTests(unittest.TestCase):
    def test_quota_error_is_returned_as_openai_429(self):
        app = FastAPI()
        app.include_router(ai_module.create_router())

        def fail(_payload):
            raise QuotaExceededError("每日额度不足，剩余 0 张", dimension="daily")

        with patch.object(ai_module, "require_identity", return_value={"id": "key-1", "role": "user", "name": "u"}), \
             patch.object(ai_module, "filter_or_log", return_value=None), \
             patch.object(ai_module.openai_v1_image_generations, "handle", fail):
            response = TestClient(app).post("/v1/images/generations", json={"prompt": "cat"})
        self.assertEqual(response.status_code, 429)
        self.assertEqual(response.json()["error"]["code"], "insufficient_quota")


if __name__ == "__main__":
    unittest.main()
