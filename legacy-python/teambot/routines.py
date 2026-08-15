"""定时例程：bot 学会的重复性任务，按固定间隔自动触发（对应 Grok Bot 的 routines）。"""

from __future__ import annotations

import json
import sys
import threading
import time
from dataclasses import asdict, dataclass

# 间隔上限一年：既挡住荒谬的输入，也保证 next_run 不会溢出 datetime 的表示范围
MAX_EVERY_MINUTES = 60 * 24 * 365


@dataclass
class Routine:
    name: str
    bot: str
    prompt: str
    every_minutes: int
    next_run: float  # epoch 秒


class Scheduler(threading.Thread):
    """后台线程：每隔几秒检查一次，到点的例程作为消息投递给对应 bot。"""

    def __init__(self, bus, path) -> None:
        super().__init__(daemon=True, name="scheduler")
        self.bus = bus
        self.path = path
        self._lock = threading.Lock()
        self._routines: dict[str, Routine] = {}
        self._load()

    # ---- 持久化 ----

    def _load(self) -> None:
        """加载 routines.json。文件可能被手工编辑过，逐条校验，坏条目跳过。"""
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError):
            return
        if not isinstance(data, list):
            print(f"⚠️ {self.path} 顶层不是列表，忽略其中内容", file=sys.stderr)
            return
        for item in data:
            try:
                r = Routine(
                    name=str(item["name"]),
                    bot=str(item["bot"]),
                    prompt=str(item["prompt"]),
                    every_minutes=self._clamp_every(item["every_minutes"]),
                    next_run=float(item["next_run"]),
                )
            except (TypeError, KeyError, ValueError, OverflowError):
                print(f"⚠️ 跳过 routines.json 中的坏条目: {item!r}", file=sys.stderr)
                continue
            self._routines[r.name] = r

    def _save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        payload = [asdict(r) for r in self._routines.values()]
        self.path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8"
        )

    @staticmethod
    def _clamp_every(value) -> int:
        return min(max(1, int(value)), MAX_EVERY_MINUTES)

    # ---- 增删查 ----

    def add(self, name: str, bot: str, prompt: str, every_minutes: int) -> Routine:
        every_minutes = self._clamp_every(every_minutes)
        r = Routine(
            name=name,
            bot=bot,
            prompt=prompt,
            every_minutes=every_minutes,
            next_run=time.time() + every_minutes * 60,
        )
        with self._lock:
            self._routines[name] = r
            self._save()
        return r

    def remove(self, name: str) -> bool:
        with self._lock:
            if name not in self._routines:
                return False
            del self._routines[name]
            self._save()
            return True

    def list(self) -> list[Routine]:
        with self._lock:
            return sorted(self._routines.values(), key=lambda r: r.next_run)

    # ---- 调度循环 ----

    def _tick(self, now: float | None = None) -> None:
        """检查一轮：把到点的例程投递出去，并推进 next_run。"""
        now = time.time() if now is None else now
        due: list[Routine] = []
        with self._lock:
            for r in self._routines.values():
                if r.next_run <= now:
                    due.append(r)
                    # 算术推进（而非 while 累加）：无论数据如何都不会死循环，
                    # 且积压的触发点直接跳到下一个未来时刻
                    step = self._clamp_every(r.every_minutes) * 60
                    r.next_run += ((now - r.next_run) // step + 1) * step
            if due:
                self._save()
        for r in due:
            delivered = self.bus.deliver(f"routine:{r.name}", r.bot, r.prompt)
            if delivered:
                self.bus.to_user("scheduler", f"⏰ 例程「{r.name}」触发，已派给 {r.bot}")
            else:
                self.bus.to_user(
                    "scheduler", f"⚠️ 例程「{r.name}」的执行者 {r.bot} 不存在，已跳过"
                )

    def run(self) -> None:
        while True:
            try:
                self._tick()
            except Exception as e:  # 调度线程绝不能死：报告后继续
                self.bus.to_user("scheduler", f"⚠️ 调度器出错：{type(e).__name__}: {e}")
            time.sleep(5)
