from __future__ import annotations

from collections import defaultdict
from datetime import datetime, time, timezone, timedelta
from typing import Any, Callable
from zoneinfo import ZoneInfo

from services.log_service import LOG_TYPE_CALL

NEW_YORK = ZoneInfo("America/New_York")
MIGRATION_VERSION = 1


def _local_datetime(value: object) -> datetime | None:
    raw = str(value or "").strip()
    if not raw:
        return None
    try:
        parsed = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        try:
            parsed = datetime.strptime(raw[:19], "%Y-%m-%d %H:%M:%S")
        except ValueError:
            return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=NEW_YORK)
    return parsed.astimezone(NEW_YORK)


def _image_count(detail: dict[str, Any]) -> int:
    recorded = detail.get("image_count")
    try:
        if recorded is not None and int(recorded) > 0:
            return int(recorded)
    except (TypeError, ValueError):
        pass
    urls = detail.get("urls")
    if isinstance(urls, list) and urls:
        return max(1, len(urls))
    data = detail.get("data")
    if isinstance(data, list) and data:
        return max(1, len(data))
    return 1


def migrate_usage_from_logs(
    auth_service: Any,
    log_service: Any,
    *,
    now_provider: Callable[[], datetime] | None = None,
) -> dict[str, int]:
    """Backfill successful image usage exactly once per key.

    Old logs without a reliable output list count as one image. A marker on
    each key makes rerunning this migration safe and idempotent.
    """
    now = (now_provider or (lambda: datetime.now(timezone.utc)))()
    if now.tzinfo is None:
        now = now.replace(tzinfo=timezone.utc)
    now = now.astimezone(NEW_YORK)
    day = now.date().isoformat()
    week = (now.date() - timedelta(days=now.weekday())).isoformat()
    counts: dict[str, int] = defaultdict(int)
    daily_counts: dict[str, int] = defaultdict(int)
    weekly_counts: dict[str, int] = defaultdict(int)
    logs = log_service.list(type=LOG_TYPE_CALL, limit=1_000_000)
    for entry in logs:
        detail = entry.get("detail") if isinstance(entry, dict) else None
        if not isinstance(detail, dict):
            continue
        endpoint = str(detail.get("endpoint") or "")
        status = str(detail.get("status") or "success").lower()
        is_image_call = endpoint.startswith("/v1/images")
        if not is_image_call and endpoint in {"/v1/chat/completions", "/v1/responses"}:
            urls = detail.get("urls")
            summary = str(entry.get("summary") or "")
            is_image_call = "文生图" in summary or "图生图" in summary or any(
                "/images/" in str(url) or "/image/" in str(url)
                for url in urls
            ) if isinstance(urls, list) else ("文生图" in summary or "图生图" in summary)
        recorded_count = detail.get("image_count")
        try:
            has_recorded_output = int(recorded_count or 0) > 0
        except (TypeError, ValueError):
            has_recorded_output = False
        if not is_image_call or (status not in {"success", "completed"} and not (status == "failed" and has_recorded_output)):
            continue
        key_id = str(detail.get("key_id") or "").strip()
        if not key_id:
            continue
        amount = _image_count(detail)
        counts[key_id] += amount
        ended = _local_datetime(detail.get("ended_at") or entry.get("time"))
        if ended is not None:
            if ended.date().isoformat() == day:
                daily_counts[key_id] += amount
            if (ended.date() - timedelta(days=ended.weekday())).isoformat() == week:
                weekly_counts[key_id] += amount

    updated = 0
    skipped = 0
    for item in auth_service.list_key_snapshots():
        key_id = str(item.get("id") or "").strip()
        if not key_id or int(item.get("usage_migration_version") or 0) >= MIGRATION_VERSION:
            skipped += 1
            continue

        def apply(target: dict[str, object], key_id: str = key_id) -> dict[str, object]:
            amount = counts.get(key_id, 0)
            target["lifetime_used"] = max(0, int(target.get("lifetime_used") or 0)) + amount
            target["total_used"] = max(0, int(target.get("total_used") or 0)) + amount
            if target.get("total_quota") is not None:
                target["total_quota"] = max(0, int(target.get("total_quota") or 0) - amount)
            target["daily_used"] = daily_counts.get(key_id, 0)
            target["weekly_used"] = weekly_counts.get(key_id, 0)
            target["daily_window_start"] = day
            target["weekly_window_start"] = week
            target["usage_migration_version"] = MIGRATION_VERSION
            return target

        if auth_service.mutate_usage_item(key_id, apply) is not None:
            updated += 1
    return {"updated": updated, "skipped": skipped, "counted": sum(counts.values())}
