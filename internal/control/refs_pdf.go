package control

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"

	"reasonix/internal/proc"
	"reasonix/internal/secrets"
)

// resolveAbsRef resolves the user-supplied @-reference path against baseDir
// and returns the absolute path plus the absolute base root to sandbox I/O
// under. With a baseDir, the path is confined under it (a relative path that
// escapes via ".." is rejected). With an empty baseDir, the path is returned
// as-is and the caller falls back to plain os.Stat/os.Open so CLI usage
// (where there is no controller-scoped workspace) keeps working.
func resolveAbsRef(path, baseDir string) (absPath, absBase string, ok bool) {
	if baseDir == "" {
		return path, "", true
	}
	absBase = baseDir
	if !filepath.IsAbs(absBase) {
		var err error
		absBase, err = filepath.Abs(absBase)
		if err != nil {
			return "", "", false
		}
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		cleaned = filepath.Join(absBase, cleaned)
	}
	rel, err := filepath.Rel(absBase, cleaned)
	if err != nil || !filepath.IsLocal(rel) {
		return "", "", false
	}
	return cleaned, absBase, true
}

func readPDFRef(path string, size int64) string {
	result, err := extractPDFText(path)
	if err != nil {
		return fmt.Sprintf("[PDF file %s, %d bytes — text extraction unavailable: %v. If this is a scanned/image-only PDF, use OCR or an available multimodal/vision tool with this path.]", path, size, err)
	}
	text := strings.TrimSpace(result.text)
	if text == "" {
		return fmt.Sprintf("[PDF file %s, %d bytes — no extractable text found. It may be scanned/image-only; use OCR or an available multimodal/vision tool with this path.]", path, size)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[PDF text extracted from %s using %s", path, result.tool)
	if result.truncated {
		fmt.Fprintf(&b, "; truncated to the first %d bytes", maxFileRefBytes)
	}
	b.WriteString("]\n")
	b.WriteString(text)
	return b.String()
}

func extractPDFTextDefault(path string) (pdfExtractResult, error) {
	var firstErr error
	if pdftotext, err := exec.LookPath("pdftotext"); err == nil {
		if text, truncated, err := runPDFTextCommand(pdftotext, []string{"-enc", "UTF-8", "-layout", path, "-"}); err == nil {
			return pdfExtractResult{text: text, tool: "pdftotext", truncated: truncated}, nil
		} else {
			firstErr = err
		}
	}
	python, err := findPython()
	if err != nil {
		if firstErr != nil {
			return pdfExtractResult{}, fmt.Errorf("pdftotext failed (%w), and Python PDF libraries are not available", firstErr)
		}
		return pdfExtractResult{}, fmt.Errorf("pdftotext and Python PDF libraries are not available")
	}
	text, truncated, err := runPDFTextCommand(python, []string{"-c", pythonPDFExtractScript, path})
	if err != nil {
		if firstErr != nil {
			return pdfExtractResult{}, fmt.Errorf("pdftotext failed (%w), Python PDF extraction failed (%w)", firstErr, err)
		}
		return pdfExtractResult{}, err
	}
	return pdfExtractResult{text: text, tool: "Python PDF library", truncated: truncated}, nil
}

func findPython() (string, error) {
	for _, name := range []string{"python3", "python", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("python not found")
}

func runPDFTextCommand(name string, args []string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pdfExtractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = secrets.ProcessEnv()
	setShellKillTree(cmd)
	cmd.WaitDelay = pdfExtractWaitDelay
	proc.HideWindow(cmd)
	var stdout limitedBuffer
	var stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	waitErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", false, fmt.Errorf("PDF text extraction timed out")
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			if stderr.Truncated() {
				msg += "\n…[truncated]…"
			}
			return "", false, fmt.Errorf("%w: %s", waitErr, msg)
		}
		return "", false, waitErr
	}
	return stdout.String(), stdout.Truncated(), nil
}

type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := maxFileRefBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func (b *limitedBuffer) Truncated() bool { return b.truncated }

const pythonPDFExtractScript = `
import sys

path = sys.argv[1]

try:
    from pypdf import PdfReader
except Exception:
    try:
        from PyPDF2 import PdfReader
    except Exception:
        PdfReader = None

if PdfReader is not None:
    reader = PdfReader(path)
    for page in reader.pages:
        text = page.extract_text() or ""
        if text:
            print(text)
    sys.exit(0)

try:
    import pdfplumber
except Exception as exc:
    raise SystemExit("no supported Python PDF library found") from exc

with pdfplumber.open(path) as pdf:
    for page in pdf.pages:
        text = page.extract_text() or ""
        if text:
            print(text)
`

func imageMime(data []byte, path string) string {
	mime := http.DetectContentType(data[:min(len(data), 512)])
	if strings.HasPrefix(mime, "image/") {
		return mime
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".tiff", ".tif":
		return "image/tiff"
	}
	return ""
}
