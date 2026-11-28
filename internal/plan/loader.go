package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	taskCheckRe = regexp.MustCompile(`^[#\s]*-\s+\[([ xX])\]\s+(?:\*\*)?([A-Z]\d{2,3})(?:\*\*)?\s*(?:—|-)?\s*(.*)$`)
	waveHeadRe  = regexp.MustCompile(`^#{2,4}\s+Wave\s+([A-Za-z0-9_-]+)\b\s*(?:—|-)?\s*(.*)$`)
	branchField = regexp.MustCompile(`(?im)^- \*\*Target branch:\*\*\s*` + "`?" + `([A-Za-z0-9_./-]+)` + "`?" + `\s*$`)
)

// Load returns the full detail for one plan ID inside root.
func Load(root, id string) (PlanDetail, error) {
	cleanID := strings.TrimSpace(id)
	if !planIDRe.MatchString(cleanID) {
		return PlanDetail{}, fmt.Errorf("%w: invalid plan id", ErrNotFound)
	}
	loc, ok := findPlan(root, cleanID)
	if !ok {
		return PlanDetail{}, ErrNotFound
	}
	if loc.markdown {
		content, err := os.ReadFile(loc.path)
		if err != nil {
			return PlanDetail{}, ErrNotFound
		}
		return parseMarkdownDetail(cleanID, string(content)), nil
	}
	return loadStructured(loc.dir, cleanID), nil
}

func findPlan(root, id string) (planLocation, bool) {
	manifestPath := filepath.Join(root, id, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		return planLocation{dir: filepath.Join(root, id), id: id, path: manifestPath}, true
	}
	markdownPath := filepath.Join(root, id+".md")
	if _, err := os.Stat(markdownPath); err == nil {
		return planLocation{dir: root, id: id, path: markdownPath, markdown: true}, true
	}
	var found planLocation
	walkPlans(root, 0, func(loc planLocation) {
		if loc.id == id && found.id == "" {
			found = loc
		}
	})
	return found, found.id != ""
}

func loadStructured(dir, id string) PlanDetail {
	_, waves, tasks := loadBundle(dir)
	mergeMarkdownStatus(filepath.Join(filepath.Dir(dir), id+".md"), &tasks)
	mergeMarkdownStatus(filepath.Join(dir, id+".md"), &tasks)
	recomputeWaveCounts(waves, tasks)
	detail := PlanDetail{
		PlanSummary: summaryFromManifest(dir, id),
		Waves:       waves,
		Tasks:       tasks,
	}
	detail.TotalTasks = len(tasks)
	detail.DoneTasks = countDone(tasks)
	detail.ProgressPct = progressPct(detail.DoneTasks, detail.TotalTasks)
	return detail
}

func recomputeWaveCounts(waves []WaveDetail, tasks []TaskItem) {
	for i := range waves {
		waves[i].TaskCount = 0
		waves[i].DoneCount = 0
		for _, task := range tasks {
			if waves[i].ID == task.WaveID {
				waves[i].TaskCount++
				if task.Status == "done" || task.Status == "skipped" {
					waves[i].DoneCount++
				}
			}
		}
	}
}

func parseMarkdownDetail(id, content string) PlanDetail {
	waves, tasks := parseMarkdown(content)
	title, branch := parseHeader(content)
	return PlanDetail{
		PlanSummary: PlanSummary{
			ID:           id,
			Title:        firstNonEmpty(title, id),
			TargetBranch: branch,
			TotalTasks:   len(tasks),
			DoneTasks:    countDone(tasks),
			WaveCount:    len(waves),
			ProgressPct:  progressPct(countDone(tasks), len(tasks)),
			SourceFile:   id + ".md",
		},
		Waves: waves,
		Tasks: tasks,
	}
}

func parseMarkdown(content string) ([]WaveDetail, []TaskItem) {
	waves := make([]WaveDetail, 0)
	tasks := make([]TaskItem, 0)
	currentWave := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if match := waveHeadRe.FindStringSubmatch(trimmed); match != nil {
			currentWave = match[1]
			waves = append(waves, WaveDetail{
				ID:    currentWave,
				Title: firstNonEmpty(match[2], currentWave),
			})
			continue
		}
		if task := parseTaskLine(trimmed, currentWave); task != nil {
			tasks = append(tasks, *task)
		}
	}
	recomputeWaveCounts(waves, tasks)
	return waves, tasks
}

func parseTaskLine(trimmed, waveID string) *TaskItem {
	match := taskCheckRe.FindStringSubmatch(trimmed)
	if match == nil {
		return nil
	}
	status := "backlog"
	if match[1] == "x" || match[1] == "X" {
		status = "done"
	}
	return &TaskItem{
		Code:   match[2],
		Title:  strings.TrimSpace(match[3]),
		Status: status,
		WaveID: waveID,
	}
}

func parseHeader(content string) (string, string) {
	title := ""
	branch := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if title == "" && strings.HasPrefix(trimmed, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		}
		if match := branchField.FindStringSubmatch(trimmed); match != nil {
			branch = match[1]
		}
	}
	return title, branch
}

func mergeMarkdownStatus(path string, tasks *[]TaskItem) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	statusByCode := readStatusMap(string(content))
	for i := range *tasks {
		if status, ok := statusByCode[(*tasks)[i].Code]; ok {
			(*tasks)[i].Status = status
		}
	}
	sort.Slice(*tasks, func(i, j int) bool { return (*tasks)[i].Code < (*tasks)[j].Code })
}

func readStatusMap(content string) map[string]string {
	statusByCode := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		match := taskCheckRe.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		if match[1] == "x" || match[1] == "X" {
			statusByCode[match[2]] = "done"
		} else {
			statusByCode[match[2]] = "backlog"
		}
	}
	return statusByCode
}
