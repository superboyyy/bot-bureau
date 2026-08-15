"""Bot：一位常驻后台线程的「AI 同事」。

每个 bot 拥有：
- 自己的收件箱（用户消息、同事消息、例程触发按序处理）
- 自己的工作目录（bash / 文件读写都限制在里面）
- 自己的长期记忆（MEMORY.md）
- 一个基于 Claude API 的智能体循环（工具调用直到任务完成）
"""

import queue
import threading

import anthropic

from . import config
from .bus import Bus, Msg
from .memory import Memory
from .tools import Toolbox


class Bot(threading.Thread):
    def __init__(self, client: anthropic.Anthropic, cfg: dict, bus: Bus, scheduler) -> None:
        super().__init__(daemon=True, name=f"bot-{cfg['name']}")
        self.client = client
        self.name_ = cfg["name"]
        self.role = cfg.get("role", "")
        self.description = cfg.get("description", "")
        self.bus = bus
        self.workspace = config.WORKSPACES_DIR / self.name_
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.memory = Memory(self.workspace / "MEMORY.md")
        self.toolbox = Toolbox(self.name_, self.workspace, self.memory, bus, scheduler)
        self.inbox: "queue.Queue[Msg]" = queue.Queue()
        self.busy = threading.Event()
        # 会话历史（仅本次进程内保留）
        self.messages: list = []

    # threading.Thread 已占用 .name 作为线程名，bot 名走 name_，再暴露一个只读别名
    @property
    def name(self) -> str:  # type: ignore[override]
        return self.name_

    @name.setter
    def name(self, value) -> None:  # Thread.__init__ 会赋值线程名，直接忽略
        pass

    # ---- 主循环：逐条处理收件箱 ----

    def run(self) -> None:
        while True:
            msg = self.inbox.get()
            self.busy.set()
            try:
                self._handle(msg)
            except anthropic.APIConnectionError:
                self.bus.to_user(self.name_, "⚠️ 网络错误，联系不上 Claude API，这条消息没有处理成功")
            except anthropic.AuthenticationError:
                self.bus.to_user(
                    self.name_,
                    "⚠️ API 认证失败：请设置 ANTHROPIC_API_KEY，或先 `ant auth login`",
                )
            except anthropic.APIStatusError as e:
                self.bus.to_user(self.name_, f"⚠️ API 错误（{e.status_code}）：{e.message}")
            except Exception as e:
                self.bus.to_user(self.name_, f"⚠️ 处理消息时出错：{type(e).__name__}: {e}")
            finally:
                self.busy.clear()

    def _handle(self, msg: Msg) -> None:
        if msg.sender == "user":
            text = msg.content
        elif msg.sender.startswith("routine:"):
            text = f"[定时例程「{msg.sender.split(':', 1)[1]}」触发] {msg.content}"
        else:
            text = f"[来自同事 {msg.sender} 的消息] {msg.content}"
        turn_start = len(self.messages)
        self.messages.append({"role": "user", "content": text})
        refused = self._agent_loop()
        if refused:
            # 整个回合回滚，避免残缺内容或未应答的 tool_use 留在历史里
            del self.messages[turn_start:]
        self._trim_history()

    # ---- 智能体循环 ----

    def _agent_loop(self) -> bool:
        """跑完一个回合。返回 True 表示该回合被安全系统拒绝，调用方应回滚历史。"""
        for _ in range(config.MAX_TOOL_ITERATIONS):
            response = self._create()

            # 安全分类器拒绝：内容可能为空或不完整，先于读取 content 处理
            if response.stop_reason == "refusal":
                self.bus.to_user(self.name_, "🚫 这个请求被模型的安全系统拒绝了，换个说法试试")
                return True

            # 整段 content 原样进历史（保留 thinking / tool_use / 服务端工具块）
            self.messages.append({"role": "assistant", "content": response.content})

            for block in response.content:
                if block.type == "text" and block.text.strip():
                    self.bus.to_user(self.name_, block.text)
                elif block.type == "server_tool_use":
                    self.bus.to_user(self.name_, f"🔎 {block.name} …")

            if response.stop_reason == "pause_turn":
                # 服务端工具轮次被暂停：原样重发即可续跑
                continue

            if response.stop_reason == "tool_use":
                results = []
                for block in response.content:
                    if block.type != "tool_use":
                        continue
                    self.bus.to_user(self.name_, f"🔧 {self._describe_tool_call(block)}")
                    content, is_error = self.toolbox.execute(block.name, block.input)
                    result: dict = {
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": content,
                    }
                    if is_error:
                        result["is_error"] = True
                    results.append(result)
                self.messages.append({"role": "user", "content": results})
                continue

            if response.stop_reason == "max_tokens":
                # 截断时可能已生成完整的 tool_use 块：必须补上配对的 tool_result，
                # 否则历史形状非法，后续每次请求都会 400，bot 永久卡死
                orphans = [b for b in response.content if b.type == "tool_use"]
                if orphans:
                    self.messages.append({
                        "role": "user",
                        "content": [
                            {
                                "type": "tool_result",
                                "tool_use_id": b.id,
                                "content": "（回复因长度限制被截断，该工具调用未执行）",
                                "is_error": True,
                            }
                            for b in orphans
                        ],
                    })
                self.bus.to_user(self.name_, "（回复太长被截断了，可以让我继续）")
            return False
        self.bus.to_user(self.name_, "（这个任务的工具调用轮数到达上限，先停在这里，需要的话让我继续）")
        return False

    def _create(self):
        return self.client.beta.messages.create(
            model=config.MODEL,
            max_tokens=config.MAX_TOKENS,
            system=self._system_prompt(),
            messages=self.messages,
            tools=self.toolbox.definitions(),
            # Opus 5 安全分类器可能拒答；开启服务端 fallback 让被拒请求自动改由推荐模型接续
            betas=["server-side-fallback-2026-07-01"],
            extra_body={"fallbacks": "default"},
        )

    @staticmethod
    def _describe_tool_call(block) -> str:
        args = block.input or {}
        if block.name == "bash":
            return f"$ {args.get('command', '')}"
        if block.name in ("read_file", "write_file"):
            return f"{block.name}: {args.get('path', '')}"
        if block.name == "message_bot":
            return f"message_bot → {args.get('to', '')}"
        if block.name == "save_routine":
            return f"save_routine: {args.get('name', '')}"
        if block.name == "remember":
            return "remember"
        return block.name

    # ---- 系统提示词 ----

    def _system_prompt(self) -> str:
        roster = "\n".join(
            f"- {b.name}（{b.role}）：{b.description}"
            for b in self.bus.bots.values()
            if b.name != self.name_
        ) or "（暂无其他成员）"
        memory = self.memory.load() or "（暂无）"
        return f"""你是 {self.name_}，用户 AI 同事团队里的{self.role}。{self.description}

你的工作方式类似 xAI 的 Grok Bot：用户像给同事发消息一样把活派给你，你独立完成多步骤任务，只在需要人工决策（例如命令审批）时停下来等待。任务完成后简明扼要地汇报结果；执行中只在有关键发现或改变方向时说一句话。

# 团队
{roster}

需要协作时用 message_bot 把子任务转交给合适的同事，消息必须自包含（对方看不到你的上下文）。收到同事消息时，完成对应工作后用 message_bot 把结果回给对方；不要来回寒暄或空洞确认，避免消息循环。

# 工作环境
- 你的工作目录：{self.workspace}（bash、read_file、write_file 都以它为根）
- 联网查资料用 web_search / web_fetch
- 非只读的 bash 命令需要用户审批，等待和被拒都是正常流程

# 长期记忆
值得跨会话保留的信息（用户偏好、踩过的坑、确认过的做法）用 remember 记下来。当前记忆：
{memory}

# 定时例程
用户希望某件事定期自动做时，用 save_routine 保存；prompt 要写成自包含的任务描述。

# 沟通
用用户的语言（默认中文）回复。汇报先说结论，再给必要的细节。"""

    # ---- 历史修剪 ----

    def _trim_history(self) -> None:
        if len(self.messages) <= config.HISTORY_LIMIT:
            return
        # 只能从「纯文本的用户消息」处切开，避免拆散 tool_use/tool_result 与 thinking 块
        cut_from = len(self.messages) - config.HISTORY_LIMIT
        for i in range(cut_from, len(self.messages)):
            m = self.messages[i]
            if m["role"] == "user" and isinstance(m.get("content"), str):
                self.messages = self.messages[i:]
                return
