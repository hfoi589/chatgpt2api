#!/usr/bin/env python3
"""Backfill image usage from existing call logs (safe to run repeatedly)."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from services.auth_service import auth_service
from services.log_service import log_service
from services.quota_migration import migrate_usage_from_logs


if __name__ == "__main__":
    print(migrate_usage_from_logs(auth_service, log_service))
