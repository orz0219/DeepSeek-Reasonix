package bot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"reasonix/internal/proc"
	"reasonix/internal/secrets"
)

type botProjectSearchResult struct {
	ProjectID   string
	ProjectName string
	Path        string
	Line        int
	Text        string
}

func formatBotProjectSearchResults(results []botProjectSearchResult, limit int) string {
	if len(results) == 0 {
		return "没有跨项目命中。"
	}
	if limit <= 0 || limit > len(results) {
		limit = len(results)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "跨项目检索结果（%d/%d）：", limit, len(results))
	for i := range limit {
		result := results[i]
		project := firstNonEmptyString(result.ProjectName, result.ProjectID)
		fmt.Fprintf(&b, "\n- %s %s:%d: %s", project, displayBotPath(result.Path), result.Line, singleLineBotText(result.Text, 120))
	}
	if len(results) > limit {
		fmt.Fprintf(&b, "\n还有 %d 条命中，请加关键词缩小范围。", len(results)-limit)
	}
	return b.String()
}

func searchBotProjects(ctx context.Context, projects []botProjectEntry, query string, limit int) ([]botProjectSearchResult, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return nil, errors.New("检索词至少需要 2 个字符")
	}
	var roots []string
	seen := map[string]bool{}
	for _, project := range projects {
		root := canonicalBotPath(project.Root)
		if root == "" || seen[root] {
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, errors.New("没有可检索的项目目录")
	}
	if limit <= 0 {
		limit = botSearchListLimit
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		return searchBotProjectsWithRG(ctx, rg, projects, roots, query, limit)
	}
	return searchBotProjectsFallback(ctx, projects, roots, query, limit)
}

func searchBotProjectsWithRG(ctx context.Context, rg string, projects []botProjectEntry, roots []string, query string, limit int) ([]botProjectSearchResult, error) {
	args := []string{
		"--json",
		"--color", "never",
		"--fixed-strings",
		"--max-count", "3",
		"--max-filesize", "1M",
		"--glob", "!.git",
		"--glob", "!node_modules",
		"--glob", "!dist",
		"--glob", "!build",
		"--glob", "!vendor",
		"--",
		query,
	}
	args = append(args, roots...)
	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Env = secrets.ProcessEnv()

	proc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg failed: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var results []botProjectSearchResult
	for {
		var item struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				Lines struct {
					Text string `json:"text"`
				} `json:"lines"`
				LineNumber int `json:"line_number"`
			} `json:"data"`
		}
		if err := dec.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			break
		}
		if item.Type != "match" {
			continue
		}
		path := canonicalBotPath(item.Data.Path.Text)
		project := botProjectForPath(projects, path)
		results = append(results, botProjectSearchResult{
			ProjectID:   project.ID,
			ProjectName: project.Name,
			Path:        path,
			Line:        item.Data.LineNumber,
			Text:        strings.TrimSpace(item.Data.Lines.Text),
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

var errStopBotSearch = errors.New("stop bot project search")

func searchBotProjectsFallback(ctx context.Context, projects []botProjectEntry, roots []string, query string, limit int) ([]botProjectSearchResult, error) {
	queryLower := strings.ToLower(query)
	var results []botProjectSearchResult
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				if shouldSkipBotSearchDir(d.Name()) && path != root {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > 1024*1024 {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return nil
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			line := 0
			for scanner.Scan() {
				if err := ctx.Err(); err != nil {
					_ = file.Close()
					return err
				}
				line++
				text := scanner.Text()
				if strings.Contains(strings.ToLower(text), queryLower) {
					project := botProjectForPath(projects, path)
					results = append(results, botProjectSearchResult{
						ProjectID:   project.ID,
						ProjectName: project.Name,
						Path:        canonicalBotPath(path),
						Line:        line,
						Text:        strings.TrimSpace(text),
					})
					if len(results) >= limit {
						_ = file.Close()
						return errStopBotSearch
					}
				}
			}
			_ = file.Close()
			return nil
		})
		if errors.Is(walkErr, errStopBotSearch) {
			break
		}
		if walkErr != nil {
			return results, walkErr
		}
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

func shouldSkipBotSearchDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build", "vendor", ".next", ".cache":
		return true
	default:
		return false
	}
}
