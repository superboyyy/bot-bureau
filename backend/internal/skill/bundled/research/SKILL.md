---
name: research
description: Look something up online and keep only what should survive. Use when the answer is not in the workspace and you need current or external facts.
---

1. If you already have a URL, fetch_url it. If you need to search, call web_search when that tool is available; otherwise ask the user for an address or work from links they gave you.
2. Read the pages you chose with fetch_url. Do not dump a whole page into memory.
3. remember only facts that should survive this conversation: scope=self for your own notes, scope=team for conventions the whole team should share. Do not record a page dump.
