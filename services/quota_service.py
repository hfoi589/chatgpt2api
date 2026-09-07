from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, time, timedelta, timezone
from typing import Any, Callable
from zoneinfo import ZoneInfo


NEW_YORK = ZoneInfo("America/New_York")


class QuotaExceededError(RuntimeError):
    """Raised when a user key cannot reserve the requested image count."""

    status_code = 429

    def __init__(self, reason: str, *, dimension: str = "quota") -> None:
        super().__init__(reason)
        self.reason = reason
        self.dimension = dimension


@dataclass(frozen=True)
class ImageQuotaReservation:
    key_id: str
    amount: int


class QuotaService:
    """Atomic per-user image quota accounting.

    Counters are persisted on the auth-key record. A reservation increments the
    periodic/lifetime counters and decrements the configured total balance before
    upstream work starts; settlement changes them by the delta between the
    reserved amount and the number of images actually returned.
    """

    def __init__(
        self,
        auth_service: Any,
        *,
        now_provider: Callable[[], datetime] | None = None,
        timezone_: ZoneInfo = NEW_YORK,
    ) -> None:
        self.auth_service = auth_service
        self.now_provider = now_provider or (lambda: datetime.now(timezone.utc))
        self.timezone = timezone_

    def _now_local(self) -> datetime:
        now = self.now_provider()
        if now.tzinfo is None:
            now = now.replace(tzinfo=timezone.utc)
        return now.astimezone(self.timezone)

    def _periods(self) -> tuple[str, str, datetime, datetime]:
        now = self._now_local()
        day = now.date()
        week = day - timedelta(days=day.weekday())
        daily_start = datetime.combine(day + timedelta(days=1), time.min, self.timezone)
        weekly_start = datetime.combine(week + timedelta(days=7), time.min, self.timezone)
        return day.isoformat(), week.isoformat(), daily_start, weekly_start

    @staticmethod
    def _as_limit(value: object) -> int | None:
        if value is None or (isinstance(value, str) and not value.strip()):
            return None
        try:
            return max(0, int(value))
        except (TypeError, ValueError):
            return None

    @staticmethod
    def _as_used(value: object) -> int:
        try:
            return max(0, int(value or 0))
        except (TypeError, ValueError):
            return 0

    @staticmethod
    def _remaining(limit: int | None, used: int) -> int | None:
        return None if limit is None else max(0, limit - used)

    @staticmethod
    def _reset_item(item: dict[str, object], day: str, week: str) -> None:
        if item.get("daily_window_start") != day:
            item["daily_window_start"] = day
            item["daily_used"] = 0
        if item.get("weekly_window_start") != week:
            item["weekly_window_start"] = week
            item["weekly_used"] = 0

    def _mutate(self, key_id: str, mutator) -> dict[str, object] | None:
        method = getattr(self.auth_service, "mutate_usage_item", None)
        if callable(method):
            return method(key_id, mutator)
        # Small fallback for test doubles and older integrations.
        item = self.auth_service.get_key_for_usage(key_id)
        if item is None:
            return None
        next_item = dict(item)
        result = mutator(next_item)
        if result is None:
            return None
        saver = getattr(self.auth_service, "save_usage_item", None)
        if callable(saver):
            saver(result)
        return result

    def reserve_images(self, identity: dict[str, object], amount: int = 1) -> ImageQuotaReservation:
        try:
            requested = int(amount)
        except (TypeError, ValueError) as exc:
            raise ValueError("图片数量必须是正整数") from exc
        if requested < 1:
            raise ValueError("图片数量必须是正整数")
        if str(identity.get("role") or "").strip().lower() == "admin":
            return ImageQuotaReservation("", requested)
        key_id = str(identity.get("id") or "").strip()
        if not key_id:
            raise QuotaExceededError("用户密钥无效", dimension="key")
        day, week, _daily_reset, _weekly_reset = self._periods()

        def apply(item: dict[str, object]) -> dict[str, object]:
            if not bool(item.get("enabled", True)):
                raise QuotaExceededError("用户密钥已禁用", dimension="enabled")
            self._reset_item(item, day, week)
            limits = {
                "daily": self._as_limit(item.get("daily_quota")),
                "weekly": self._as_limit(item.get("weekly_quota")),
                "total": self._as_limit(item.get("total_quota")),
            }
            used = {
                "daily": self._as_used(item.get("daily_used")),
                "weekly": self._as_used(item.get("weekly_used")),
                "total": self._as_used(item.get("total_used")),
            }
            labels = {"daily": "每日额度", "weekly": "每周额度", "total": "总额度"}
            for dimension, limit in limits.items():
                remaining = limit if dimension == "total" else self._remaining(limit, used[dimension])
                if remaining is not None and remaining < requested:
                    raise QuotaExceededError(f"{labels[dimension]}不足，剩余 {remaining} 张", dimension=dimension)
            item["daily_used"] = used["daily"] + requested
            item["weekly_used"] = used["weekly"] + requested
            # total_quota is a remaining balance, unlike the periodic limits.
            if limits["total"] is not None:
                item["total_quota"] = limits["total"] - requested
            item["total_used"] = used["total"] + requested
            item["lifetime_used"] = self._as_used(item.get("lifetime_used")) + requested
            return item

        updated = self._mutate(key_id, apply)
        if updated is None:
            raise QuotaExceededError("用户密钥不存在", dimension="key")
        return ImageQuotaReservation(key_id, requested)

    def settle_images(self, reservation: ImageQuotaReservation | None, actual: int) -> None:
        if reservation is None or not reservation.key_id:
            return
        try:
            completed = max(0, min(int(actual), reservation.amount))
        except (TypeError, ValueError):
            completed = 0
        delta = completed - reservation.amount
        if delta == 0:
            return

        def apply(item: dict[str, object]) -> dict[str, object]:
            for field in ("daily_used", "weekly_used", "total_used", "lifetime_used"):
                item[field] = max(0, self._as_used(item.get(field)) + delta)
            if item.get("total_quota") is not None and delta < 0:
                item["total_quota"] = self._as_used(item.get("total_quota")) - delta
            return item

        self._mutate(reservation.key_id, apply)

    def public_usage(self, identity: dict[str, object]) -> dict[str, object]:
        if str(identity.get("role") or "").strip().lower() == "admin":
            return {
                "lifetime_used": 0,
                "daily": {"used": 0, "limit": None, "remaining": None, "reset_at": None},
                "weekly": {"used": 0, "limit": None, "remaining": None, "reset_at": None},
                "total": {"used": 0, "limit": None, "remaining": None},
                "blocked": False,
                "reason": None,
                "route_account_ids": [],
            }
        key_id = str(identity.get("id") or "").strip()
        day, week, daily_reset, weekly_reset = self._periods()

        def apply(item: dict[str, object]) -> dict[str, object]:
            self._reset_item(item, day, week)
            return item

        item = self._mutate(key_id, apply)
        if item is None:
            return {
                "lifetime_used": 0,
                "daily": {"used": 0, "limit": None, "remaining": None, "reset_at": daily_reset.isoformat()},
                "weekly": {"used": 0, "limit": None, "remaining": None, "reset_at": weekly_reset.isoformat()},
                "total": {"used": 0, "limit": 0, "remaining": 0},
                "blocked": True,
                "reason": "用户密钥不存在",
                "route_account_ids": [],
            }
        daily_limit = self._as_limit(item.get("daily_quota"))
        weekly_limit = self._as_limit(item.get("weekly_quota"))
        total_limit = self._as_limit(item.get("total_quota"))
        daily_used = self._as_used(item.get("daily_used"))
        weekly_used = self._as_used(item.get("weekly_used"))
        total_used = self._as_used(item.get("total_used"))
        daily_remaining = self._remaining(daily_limit, daily_used)
        weekly_remaining = self._remaining(weekly_limit, weekly_used)
        total_remaining = total_limit
        total_baseline = None if total_limit is None else total_limit + total_used
        reason = None
        if not bool(item.get("enabled", True)):
            reason = "用户密钥已禁用"
        elif daily_remaining is not None and daily_remaining <= 0:
            reason = "每日额度已用尽"
        elif weekly_remaining is not None and weekly_remaining <= 0:
            reason = "每周额度已用尽"
        elif total_remaining is not None and total_remaining <= 0:
            reason = "总额度已用尽"
        return {
            "lifetime_used": self._as_used(item.get("lifetime_used")),
            "daily": {
                "used": daily_used,
                "limit": daily_limit,
                "remaining": daily_remaining,
                "reset_at": daily_reset.isoformat(),
            },
            "weekly": {
                "used": weekly_used,
                "limit": weekly_limit,
                "remaining": weekly_remaining,
                "reset_at": weekly_reset.isoformat(),
            },
            "total": {"used": total_used, "limit": total_baseline, "remaining": total_remaining},
            "blocked": reason is not None,
            "reason": reason,
            "route_account_ids": list(item.get("route_account_ids") or []),
        }

    def route_account_ids(self, identity: dict[str, object]) -> list[str]:
        if str(identity.get("role") or "").strip().lower() == "admin":
            return []
        key_id = str(identity.get("id") or "").strip()
        item = self.auth_service.get_key_for_usage(key_id) if key_id else None
        routes = item.get("route_account_ids") if isinstance(item, dict) else identity.get("route_account_ids")
        return list(dict.fromkeys(str(item).strip() for item in routes if str(item).strip())) if isinstance(routes, list) else []


from services.auth_service import auth_service

quota_service = QuotaService(auth_service)
