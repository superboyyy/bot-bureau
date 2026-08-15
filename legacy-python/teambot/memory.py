"""每个 bot 的长期记忆：工作目录下的一个 MEMORY.md，跨会话保留。"""

import datetime
import threading
from pathlib import Path


class Memory:
    def __init__(self, path: Path) -> None:
        self.path = path
        self._lock = threading.Lock()

    def load(self) -> str:
        try:
            return self.path.read_text(encoding="utf-8").strip()
        except FileNotFoundError:
            return ""

    def append(self, note: str) -> None:
        stamp = datetime.date.today().isoformat()
        line = f"- [{stamp}] {note.strip()}\n"
        with self._lock:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            with open(self.path, "a", encoding="utf-8") as f:
                f.write(line)
