---
name: documents
description: Read or create Word, Excel, and PowerPoint files (.docx, .xlsx, .pptx). Use when the user attaches an office document, asks about a spreadsheet or slide deck, or wants one written.
---

read_file extracts text from .docx, .xlsx and .pptx (including inbox/ attachments). Sheets and slides are labelled in the extracted text.

1. Call this skill, then read_file the path. Use offset/limit on long files.
2. grep searches the extracted text. Prefer it over unzip or bash.
3. If extraction is empty, say so; do not invent cells or slides.
4. To create a document, write markdown or CSV in the workspace first. Convert with python-docx, openpyxl, or python-pptx when those libraries are installed; otherwise leave the markdown/CSV and tell the user. Conversion is bash and needs approval.
5. Never edit_file a .docx/.xlsx/.pptx; they are zip packages, not text. Round-trip through extract → edit a copy as text → convert back.
