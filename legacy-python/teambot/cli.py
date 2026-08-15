"""终端聊天界面：像给同事发消息一样和你的 AI 团队交流。"""

import datetime
import sys
import threading

import anthropic

from . import config
from .bot import Bot
from .bus import Bus
from .routines import Scheduler

RESET = "\033[0m"
DIM = "\033[2m"
BOLD = "\033[1m"
PALETTE = ["\033[36m", "\033[33m", "\033[35m", "\033[32m", "\033[34m", "\033[31m"]

HELP = """可用输入：
  直接输入文字            发给默认收件人 {default}
  @bot名 消息             发给指定 bot（如 @scout 查一下…）
  /bots                  查看团队成员与状态
  /approve <编号>         批准一个待审批的命令
  /deny <编号> [原因]      拒绝一个待审批的命令
  /pending               列出待审批项
  /routines              查看定时例程
  /routines rm <名称>     删除某个例程
  /memory <bot名>         查看某个 bot 的长期记忆
  /help                  显示本帮助
  /quit                  退出"""


def _printer(bus: Bus, colors: dict[str, str]) -> None:
    # 这是 bus.output 的唯一消费者，也是审批提示的唯一出口——循环体绝不能因异常退出
    while True:
        try:
            source, text = bus.output.get()
            color = colors.get(source, DIM)
            print(f"\n{color}{BOLD}● {source}{RESET} {text}")
        except Exception as e:
            print(f"[printer 输出失败: {e!r}]", file=sys.stderr)


def main() -> None:
    # 终端编码不支持 emoji 时用替换字符而非抛 UnicodeEncodeError
    try:
        sys.stdout.reconfigure(errors="replace")
    except (AttributeError, ValueError):
        pass
    try:
        client = anthropic.Anthropic()
    except anthropic.AnthropicError as e:
        raise SystemExit(f"初始化 Anthropic 客户端失败：{e}\n请设置 ANTHROPIC_API_KEY 或先 `ant auth login`")

    bus = Bus()
    scheduler = Scheduler(bus, config.ROUTINES_FILE)
    bot_cfgs = config.load_bot_configs()
    bots = [Bot(client, cfg, bus, scheduler) for cfg in bot_cfgs]
    for b in bots:
        bus.register(b)
    default_bot = bots[0].name

    colors = {b.name: PALETTE[i % len(PALETTE)] for i, b in enumerate(bots)}
    colors["scheduler"] = DIM
    threading.Thread(target=_printer, args=(bus, colors), daemon=True).start()
    for b in bots:
        b.start()
    scheduler.start()

    roster = "  ".join(f"{colors[b.name]}●{RESET} {b.name}（{b.role}）" for b in bots)
    print(f"{BOLD}╭─ teambot · 本地版 Grok Bot ─╮{RESET}")
    print(f"团队：{roster}")
    print(f"直接输入 → 发给 {default_bot}；@bot名 消息 → 指定成员；/help 查看命令\n")

    while True:
        try:
            line = input().strip()
        except (EOFError, KeyboardInterrupt):
            print("\n再见 👋")
            return
        if not line:
            continue
        if line.startswith("/"):
            try:
                if _command(line, bus, scheduler, default_bot):
                    return
            except Exception as e:  # 单条命令出错不应让整个 CLI 退出
                print(f"  ⚠️ 命令执行出错：{type(e).__name__}: {e}")
        elif line.startswith("@"):
            target, _, content = line[1:].partition(" ")
            if target in bus.bots and content.strip():
                bus.deliver("user", target, content.strip())
            elif target not in bus.bots:
                print(f"{DIM}没有叫 {target} 的 bot，可用：{', '.join(bus.bots)}{RESET}")
            else:
                print(f"{DIM}用法：@{target} 消息内容{RESET}")
        else:
            bus.deliver("user", default_bot, line)


def _command(line: str, bus: Bus, scheduler: Scheduler, default_bot: str) -> bool:
    """处理 / 命令。返回 True 表示退出程序。"""
    parts = line.split()
    cmd, args = parts[0].lower(), parts[1:]

    if cmd in ("/quit", "/exit", "/q"):
        print("再见 👋")
        return True

    if cmd == "/help":
        print(HELP.format(default=default_bot))

    elif cmd == "/bots":
        for b in bus.bots.values():
            state = "🔵 工作中" if b.busy.is_set() else ("… 排队中" if not b.inbox.empty() else "⚪ 空闲")
            print(f"  {b.name}（{b.role}） {state}   工作目录: {b.workspace}")

    elif cmd == "/pending":
        pending = bus.pending_approvals()
        if not pending:
            print("  （没有待审批项）")
        for r in pending:
            print(f"  #{r.id} [{r.bot}] {r.action}")

    elif cmd in ("/approve", "/deny"):
        if not args or not args[0].isdigit():
            print(f"  用法：{cmd} <编号>")
            return False
        approved = cmd == "/approve"
        reason = " ".join(args[1:])
        if bus.decide(int(args[0]), approved, reason):
            print(f"  已{'批准' if approved else '拒绝'} #{args[0]}")
        else:
            print(f"  找不到待审批项 #{args[0]}（可能已处理过），/pending 查看")

    elif cmd == "/routines":
        if args and args[0] == "rm":
            if len(args) < 2:
                print("  用法：/routines rm <名称>")
            elif scheduler.remove(args[1]):
                print(f"  已删除例程「{args[1]}」")
            else:
                print(f"  没有叫「{args[1]}」的例程")
            return False
        routines = scheduler.list()
        if not routines:
            print("  （没有定时例程。对 bot 说「每隔…做…」就能创建）")
        for r in routines:
            try:
                nxt = datetime.datetime.fromtimestamp(r.next_run).strftime("%m-%d %H:%M")
            except (OverflowError, ValueError, OSError, TypeError):
                nxt = "(时间无效)"
            print(f"  「{r.name}」→ {r.bot}，每 {r.every_minutes} 分钟，下次 {nxt}：{r.prompt[:60]}")

    elif cmd == "/memory":
        if not args:
            print("  用法：/memory <bot名>")
        elif args[0] not in bus.bots:
            print(f"  没有叫 {args[0]} 的 bot")
        else:
            print(bus.bots[args[0]].memory.load() or "  （记忆为空）")

    else:
        print(f"  未知命令 {cmd}，/help 查看用法")
    return False
