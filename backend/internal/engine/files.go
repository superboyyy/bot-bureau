package engine

import (
	"botbureau/backend/internal/config"
	"botbureau/backend/internal/docparse"
	"botbureau/backend/internal/i18n"
	"botbureau/backend/internal/textutil"

	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (t *Toolbox) runReadFile(rel string, offset, limit int, haveOffset, haveLimit bool) (string, bool) {
	p, err := t.resolve(rel)
	if err != nil {
		return err.Error(), true
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return i18n.T("Read failed: ") + err.Error(), true
	}
	body, errMsg := workspaceFileText(rel, raw)
	if errMsg != "" {
		return errMsg, true
	}
	if !haveOffset && !haveLimit {
		return truncateOutput(body), false
	}
	lines := splitFileLines(body)
	total := len(lines)
	start := 1
	if haveOffset {
		start = offset
	}
	if start < 1 {
		return i18n.T("offset is 1-based and must be at least 1"), true
	}
	if start > total {
		return fmt.Sprintf(i18n.T("%s has %d lines; offset %d is past the end"), rel, total, start), true
	}
	end := total
	if haveLimit {
		if limit < 1 {
			return i18n.T("limit must be a positive line count"), true
		}
		end = start + limit - 1
		if end > total {
			end = total
		}
	}
	width := len(fmt.Sprintf("%d", total))
	var b strings.Builder
	fmt.Fprintf(&b, i18n.T("%s: lines %d-%d of %d\n"), rel, start, end, total)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%*d|%s\n", width, i, lines[i-1])
	}
	return truncateOutput(strings.TrimSuffix(b.String(), "\n")), false
}

// workspaceFileText turns workspace bytes into something the model can read. PDF and
// Office files are extracted; other binary files are refused.
func workspaceFileText(rel string, raw []byte) (string, string) {
	if docparse.Detect(rel, raw) != "" {
		res, err := docparse.Extract(rel, raw)
		if err != nil {
			k := docparse.Detect(rel, raw)
			switch {
			case errors.Is(err, docparse.ErrNoText):
				return "", fmt.Sprintf(i18n.T("Extracted no text from %s (%s). It may be scanned or image-only."), rel, k)
			case errors.Is(err, docparse.ErrEncrypted):
				return "", i18n.T("That PDF is encrypted; this tool cannot extract its text.")
			default:
				return "", fmt.Sprintf(i18n.T("Could not extract text from %s: %s"), rel, err.Error())
			}
		}
		if res.Pages > 0 {
			return fmt.Sprintf(i18n.T("Extracted text from %s (%s, %d pages)\n\n"), rel, res.Kind, res.Pages) + res.Text, ""
		}
		return fmt.Sprintf(i18n.T("Extracted text from %s (%s)\n\n"), rel, res.Kind) + res.Text, ""
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", i18n.T("That file looks binary; this tool only reads text (PDF and Office documents are extracted automatically).")
	}
	return string(raw), ""
}

func (t *Toolbox) runWriteFile(rel, content string) (string, bool) {
	p, err := t.resolve(rel)
	if err != nil {
		return err.Error(), true
	}
	prev, _ := os.ReadFile(p)
	diff := textutil.Unified(rel, string(prev), content, config.ApprovalDiffLimit)
	action := fmt.Sprintf("write_file: %s (%d bytes)", rel, len(content))

	act := config.ToolAct{Kind: config.ActWrite}
	if reason, rejected, _ := t.gatePath(act, action,
		i18n.T("File write requested, waiting for approval #%d: ")+rel, diff, rel); rejected {
		return denied(i18n.T("The user rejected this file write"), reason), true
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return i18n.T("Failed to create directory: ") + err.Error(), true
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return i18n.T("Write failed: ") + err.Error(), true
	}
	return fmt.Sprintf(i18n.T("Wrote %s (%d bytes)"), rel, len(content)), false
}

func (t *Toolbox) runEditFile(rel, oldStr, newStr string, replaceAll bool) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return i18n.T("Path is empty"), true
	}
	if oldStr == "" {
		return i18n.T("old_string is empty"), true
	}
	p, err := t.resolve(rel)
	if err != nil {
		return err.Error(), true
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf(i18n.T("No such file: %s. Use write_file to create one."), rel), true
		}
		return i18n.T("Read failed: ") + err.Error(), true
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return i18n.T("That file looks binary; this tool only edits text."), true
	}
	src := string(raw)
	n := strings.Count(src, oldStr)
	if n == 0 {
		return fmt.Sprintf(i18n.T("old_string was not found in %s"), rel), true
	}
	if n > 1 && !replaceAll {
		return fmt.Sprintf(i18n.T("old_string matched %d times in %s. Pass replace_all to change every one, or add more surrounding lines so it matches once."), n, rel), true
	}
	var next string
	count := n
	if replaceAll {
		next = strings.ReplaceAll(src, oldStr, newStr)
	} else {
		next = strings.Replace(src, oldStr, newStr, 1)
		count = 1
	}
	diff := textutil.Unified(rel, src, next, config.ApprovalDiffLimit)
	action := fmt.Sprintf("edit_file: %s", rel)
	act := config.ToolAct{Kind: config.ActWrite}
	if reason, rejected, _ := t.gatePath(act, action,
		i18n.T("File edit requested, waiting for approval #%d: ")+rel, diff, rel); rejected {
		return denied(i18n.T("The user rejected this file edit"), reason), true
	}
	if err := os.WriteFile(p, []byte(next), 0o644); err != nil {
		return i18n.T("Write failed: ") + err.Error(), true
	}
	if count == 1 {
		return fmt.Sprintf(i18n.T("Edited %s (%d replacement)"), rel, count), false
	}
	return fmt.Sprintf(i18n.T("Edited %s (%d replacements)"), rel, count), false
}

func splitFileLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
