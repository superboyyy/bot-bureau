"""全局配置与 bots.yaml 加载。"""

from pathlib import Path

import yaml

# 模型与请求参数
MODEL = "claude-opus-5"
# 注意：Opus 5 默认开启思考，思考 token 也计入 max_tokens，不要设得太小
MAX_TOKENS = 16000
# 单条消息触发的智能体循环里，最多允许多少轮 API 往返（防失控）
MAX_TOOL_ITERATIONS = 30
# 会话历史超过这个消息数后，从最早的完整用户轮开始截断
HISTORY_LIMIT = 60

# bash 命令超时（秒）与工具输出截断长度（字符）
BASH_TIMEOUT = 120
TOOL_OUTPUT_LIMIT = 20000

PROJECT_ROOT = Path(__file__).resolve().parent.parent
BOTS_FILE = PROJECT_ROOT / "bots.yaml"
DATA_DIR = PROJECT_ROOT / "data"
WORKSPACES_DIR = DATA_DIR / "workspaces"
ROUTINES_FILE = DATA_DIR / "routines.json"


def load_bot_configs() -> list[dict]:
    """读取 bots.yaml，返回 [{name, role, description}, ...]。"""
    with open(BOTS_FILE, encoding="utf-8") as f:
        data = yaml.safe_load(f)
    bots = data.get("bots") or []
    if not bots:
        raise SystemExit("bots.yaml 里没有定义任何 bot")
    names = [b["name"] for b in bots]
    if len(names) != len(set(names)):
        raise SystemExit("bots.yaml 里有重名的 bot")
    return bots
