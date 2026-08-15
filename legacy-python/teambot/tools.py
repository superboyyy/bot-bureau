"""每个 bot 的工具箱：客户端工具的定义与执行，外加服务端联网工具的声明。

客户端工具（本进程执行）：bash / read_file / write_file / remember / message_bot / save_routine
服务端工具（Anthropic 侧执行，无需本地实现）：web_search / web_fetch
"""

import re
import subprocess
from pathlib import Path

from . import config

# 以这些词开头且不含 shell 元字符的 bash 命令视为只读，直接执行；其余命令先提交用户审批。
# find 不在名单里：它自带 -delete/-exec 等破坏性动作，无需任何元字符即可绕过审批门
SAFE_BASH_COMMANDS = {
    "ls", "cat", "head", "tail", "grep", "wc", "pwd", "echo", "date", "du", "file", "diff",
}
# 管道/重定向/命令替换等都可能让"只读"首词携带副作用（如 ls $(rm -rf …)、cat x > y）
_SHELL_META = re.compile(r"[;&|`$<>]|\n")


def _truncate(text: str) -> str:
    if len(text) > config.TOOL_OUTPUT_LIMIT:
        return text[: config.TOOL_OUTPUT_LIMIT] + "\n...(输出过长，已截断)"
    return text


class Toolbox:
    def __init__(self, bot_name: str, workspace: Path, memory, bus, scheduler) -> None:
        self.bot_name = bot_name
        self.workspace = workspace.resolve()
        self.memory = memory
        self.bus = bus
        self.scheduler = scheduler

    # ---- 工具定义 ----

    def definitions(self) -> list[dict]:
        teammates = [n for n in self.bus.bots if n != self.bot_name]
        tools: list[dict] = [
            {
                "name": "bash",
                "description": (
                    "在你的工作目录里执行 shell 命令。只读类命令（ls/cat/grep/find 等）会直接执行；"
                    "其余命令会先提交给用户审批，批准后才执行——审批可能需要等一会儿，属于正常流程。"
                    "被拒绝时请尊重用户决定，换一种方式或说明原因。"
                ),
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "command": {"type": "string", "description": "要执行的 shell 命令"}
                    },
                    "required": ["command"],
                },
            },
            {
                "name": "read_file",
                "description": "读取工作目录里的一个文本文件。",
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "相对工作目录的路径"}
                    },
                    "required": ["path"],
                },
            },
            {
                "name": "write_file",
                "description": "在工作目录里写入（覆盖）一个文本文件，父目录会自动创建。",
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string", "description": "相对工作目录的路径"},
                        "content": {"type": "string", "description": "完整文件内容"},
                    },
                    "required": ["path", "content"],
                },
            },
            {
                "name": "remember",
                "description": (
                    "把一条值得跨会话保留的信息写进你的长期记忆（用户偏好、踩过的坑、确认过的做法等）。"
                    "已经记住的内容不要重复记。"
                ),
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "note": {"type": "string", "description": "一句话记录要点及原因"}
                    },
                    "required": ["note"],
                },
            },
            {
                "name": "save_routine",
                "description": (
                    "当用户希望某件事「定期 / 每隔一段时间」自动做时，保存为定时例程。"
                    "到点后 prompt 会作为新任务自动发给指定 bot（默认是你自己）。"
                ),
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string", "description": "例程名（简短、唯一，同名会覆盖）"},
                        "prompt": {
                            "type": "string",
                            "description": "到点时要执行的任务描述，需自包含、不依赖本次对话上下文",
                        },
                        "every_minutes": {
                            "type": "integer",
                            "description": "间隔分钟数，例如每小时=60、每天=1440",
                        },
                    },
                    "required": ["name", "prompt", "every_minutes"],
                },
            },
        ]
        if teammates:
            tools.append(
                {
                    "name": "message_bot",
                    "description": (
                        "给团队里的另一位 AI 同事发消息：转交子任务、汇报结果或提问。"
                        "消息要自包含（对方看不到你的对话历史）。发送后对方会异步处理，"
                        "其回复会作为新消息送达你，不要原地空等。"
                    ),
                    "input_schema": {
                        "type": "object",
                        "properties": {
                            "to": {"type": "string", "enum": teammates, "description": "收件 bot"},
                            "content": {"type": "string", "description": "消息内容"},
                        },
                        "required": ["to", "content"],
                    },
                }
            )
        # 服务端联网工具：由 Anthropic 执行，结果直接出现在响应里
        tools += [
            {"type": "web_search_20260209", "name": "web_search"},
            {"type": "web_fetch_20260209", "name": "web_fetch"},
        ]
        return tools

    # ---- 工具执行（返回 (结果文本, 是否出错)） ----

    def execute(self, name: str, tool_input: dict) -> tuple[str, bool]:
        handler = getattr(self, f"_run_{name}", None)
        if handler is None:
            return f"未知工具: {name}", True
        try:
            return handler(**tool_input)
        except TypeError as e:
            return f"工具参数不合法: {e}", True
        except Exception as e:  # 工具失败不应拖垮 bot 线程
            return f"工具执行出错: {type(e).__name__}: {e}", True

    def _resolve(self, path: str) -> Path:
        p = (self.workspace / path).resolve()
        if p != self.workspace and self.workspace not in p.parents:
            raise ValueError(f"路径越出了工作目录: {path}")
        return p

    def _run_bash(self, command: str) -> tuple[str, bool]:
        stripped = command.strip()
        if not stripped:
            return "命令为空", True
        first_word = stripped.split()[0]
        is_readonly = first_word in SAFE_BASH_COMMANDS and not _SHELL_META.search(stripped)
        if not is_readonly:
            req = self.bus.request_approval(self.bot_name, f"bash: {stripped}")
            self.bus.to_user(
                self.bot_name,
                f"⏸ 请求执行命令，等待你的审批 #{req.id}：\n"
                f"    $ {stripped}\n"
                f"    （/approve {req.id} 批准，/deny {req.id} [原因] 拒绝）",
            )
            req.decided.wait()
            if not req.approved:
                why = f"，原因：{req.reason}" if req.reason else ""
                return f"用户拒绝执行该命令{why}", True
        try:
            proc = subprocess.run(
                stripped,
                shell=True,
                cwd=self.workspace,
                capture_output=True,
                text=True,
                timeout=config.BASH_TIMEOUT,
            )
        except subprocess.TimeoutExpired:
            return f"命令超时（{config.BASH_TIMEOUT} 秒）", True
        out = (proc.stdout + proc.stderr).strip() or "(无输出)"
        out = _truncate(out)
        if proc.returncode != 0:
            return f"退出码 {proc.returncode}\n{out}", True
        return out, False

    def _run_read_file(self, path: str) -> tuple[str, bool]:
        p = self._resolve(path)
        if not p.is_file():
            return f"文件不存在: {path}", True
        return _truncate(p.read_text(encoding="utf-8", errors="replace")), False

    def _run_write_file(self, path: str, content: str) -> tuple[str, bool]:
        p = self._resolve(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content, encoding="utf-8")
        return f"已写入 {path}（{len(content)} 字符）", False

    def _run_remember(self, note: str) -> tuple[str, bool]:
        self.memory.append(note)
        return "已写入长期记忆", False

    def _run_message_bot(self, to: str, content: str) -> tuple[str, bool]:
        if to == self.bot_name:
            return "不能给自己发消息", True
        if not self.bus.deliver(self.bot_name, to, content):
            return f"团队里没有叫 {to} 的 bot", True
        self.bus.to_user(self.bot_name, f"📨 已转交给 {to}：{_truncate(content[:200])}")
        return f"已发送给 {to}，对方处理完会回复你", False

    def _run_save_routine(self, name: str, prompt: str, every_minutes: int) -> tuple[str, bool]:
        r = self.scheduler.add(name=name, bot=self.bot_name, prompt=prompt, every_minutes=every_minutes)
        self.bus.to_user(
            self.bot_name, f"⏰ 已保存例程「{r.name}」：每 {r.every_minutes} 分钟执行一次"
        )
        return f"例程「{r.name}」已保存，每 {r.every_minutes} 分钟触发一次", False
