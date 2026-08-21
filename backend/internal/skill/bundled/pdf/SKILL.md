---
name: pdf
description: Read, summarise, extract from, or create PDF files. Use when the user attaches a .pdf, names a PDF in the workspace or inbox, asks to fill a form, merge or split pages, or wants a PDF written.
---

read_file extracts text from a PDF (including files the user attached under inbox/). Do not treat it as opaque binary.

1. Call this skill, then read_file the path. Use offset/limit on long documents; do not paste the whole file into the reply.
2. Page markers look like "--- page N ---". Cite page numbers when you quote.
3. If extraction is empty, the PDF is scanned or image-only — say so. Do not invent text. Ask for a screenshot or OCR.
4. grep also searches extracted PDF text. Prefer it over bash.
5. Creating, merging, splitting, or filling a PDF is a write: draft the content as markdown in the workspace first, then convert with whatever is on the machine (python with pypdf/reportlab, pandoc, qpdf). Put the output in the workspace and tell the user the path. That conversion is bash and needs approval.
6. Never edit_file a .pdf; it is not text.
