---
name: verify
description: Run the project's tests after code changes. Use after edit_file or write_file, before telling the user you are done.
---

After changing code, run the tests this project already has. Guessing that an edit worked is not verification.

- Go: `go test` on the package you touched, or `go test ./...` from the module root.
- JavaScript/TypeScript: `npm test` when package.json defines it.
- Otherwise look for a Makefile, justfile, or README section that says how to test.

Use bash. A failing test is the result; do not "fix" a test by deleting it. Report the failure and the files you changed.
