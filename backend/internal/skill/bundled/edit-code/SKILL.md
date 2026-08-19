---
name: edit-code
description: Change existing code in a repository. Use when editing, fixing, or refactoring files that are already there.
---

When changing code that already exists:

1. Find the site with grep and glob. Do not shell out to rg or find.
2. Read only the window you need with read_file offset/limit.
3. Prefer edit_file over write_file. If old_string is not unique, read more context and retry with a larger unique snippet. Never create a file with edit_file; new files stay write_file.
4. If the work will touch more than one file, call todo_write with a checklist, then submit_plan, and wait for the user to accept the plan before editing.
5. After the edits, follow the verify skill: run the project's tests.
