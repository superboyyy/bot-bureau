"""消息总线：bot 注册、消息投递、审批队列、面向用户的输出队列。"""

import itertools
import queue
import threading
from dataclasses import dataclass, field


@dataclass
class Msg:
    """投递给某个 bot 的一条消息。sender 是 "user"、某个 bot 名或 "routine:<例程名>"。"""

    sender: str
    content: str


@dataclass
class ApprovalRequest:
    """一次待用户审批的敏感操作（如非只读的 bash 命令）。"""

    id: int
    bot: str
    action: str
    decided: threading.Event = field(default_factory=threading.Event)
    approved: bool = False
    reason: str = ""


class Bus:
    def __init__(self) -> None:
        self.bots: dict = {}
        # (来源名, 文本) —— CLI 的打印线程从这里取内容展示给用户
        self.output: "queue.Queue" = queue.Queue()
        self._approvals: dict = {}
        self._approval_ids = itertools.count(1)
        self._lock = threading.Lock()

    # ---- bot 注册与消息投递 ----

    def register(self, bot) -> None:
        self.bots[bot.name] = bot

    def deliver(self, sender: str, target: str, content: str) -> bool:
        """把消息放进目标 bot 的收件箱。目标不存在时返回 False。"""
        bot = self.bots.get(target)
        if bot is None:
            return False
        bot.inbox.put(Msg(sender=sender, content=content))
        return True

    # ---- 面向用户的输出 ----

    def to_user(self, source: str, text: str) -> None:
        self.output.put((source, text))

    # ---- 审批 ----

    def request_approval(self, bot_name: str, action: str) -> ApprovalRequest:
        with self._lock:
            req = ApprovalRequest(id=next(self._approval_ids), bot=bot_name, action=action)
            self._approvals[req.id] = req
        return req

    def decide(self, approval_id: int, approved: bool, reason: str = "") -> bool:
        """用户批准/拒绝某个审批。找不到或已处理过时返回 False。"""
        with self._lock:
            req = self._approvals.get(approval_id)
            if req is None or req.decided.is_set():
                return False
            req.approved = approved
            req.reason = reason
        req.decided.set()
        return True

    def pending_approvals(self) -> list:
        with self._lock:
            return [r for r in self._approvals.values() if not r.decided.is_set()]
