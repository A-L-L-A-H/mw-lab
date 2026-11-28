package plan

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrNotFound is returned when no plan matches the requested ID.
	ErrNotFound = errors.New("plan not found")
	planIDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	skipDirs    = map[string]bool{
		".brain": true, "memory": true, "worktrees": true, "orphans": true, "T01": true, "test-plan": true,
	}
)

type planLocation struct {
	dir      string
	id       string
	path     string
	markdown bool
}

// Discover scans root recursively for plan bundles and master markdown checklists.
func Discover(root string) ([]PlanSummary, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return []PlanSummary{}, nil
	}
	if _, err := os.ReadDir(trimmed); err != nil {
		if os.IsNotExist(err) {
			return []PlanSummary{}, nil
		}
		return nil, err
	}
	result := make([]PlanSummary, 0)
	seen := make(map[string]bool)
	walkPlans(trimmed, 0, func(loc planLocation) {
		if seen[loc.id] {
			return
		}
		summary := summarizeLocation(loc)
		if summary.TotalTasks == 0 {
			return
		}
		result = append(result, summary)
		seen[loc.id] = true
	})
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func walkPlans(dir string, depth int, visit func(planLocation)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	siblingMD := collectMarkdownPlanIDs(entries)
	childDirs := visitManifestDirs(dir, entries, visit)
	for _, id := range siblingMD {
		visit(planLocation{dir: dir, id: id, path: filepath.Join(dir, id+".md"), markdown: true})
	}
	if depth >= 3 {
		return
	}
	mdSet := toSet(siblingMD)
	for _, child := range childDirs {
		if !mdSet[filepath.Base(child)] {
			walkPlans(child, depth+1, visit)
		}
	}
}

func collectMarkdownPlanIDs(entries []os.DirEntry) []string {
	ids := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		id := trimMarkdownSuffix(name)
		if planIDRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func visitManifestDirs(dir string, entries []os.DirEntry, visit func(planLocation)) []string {
	recurse := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || skipDirs[name] {
			continue
		}
		child := filepath.Join(dir, name)
		manifestPath := filepath.Join(child, "manifest.json")
		if _, statErr := os.Stat(manifestPath); statErr == nil {
			visit(planLocation{dir: child, id: name, path: manifestPath})
			continue
		}
		recurse = append(recurse, child)
	}
	return recurse
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

func summarizeLocation(loc planLocation) PlanSummary {
	if loc.markdown {
		return summaryFromMarkdown(loc.path, loc.id)
	}
	return summaryFromManifest(loc.dir, loc.id)
}

func trimMarkdownSuffix(name string) string {
	return strings.TrimSuffix(name, ".md")
}

func summaryFromManifest(dir, id string) PlanSummary {
	manifest, waves, tasks := loadBundle(dir)
	summary := PlanSummary{
		ID:           id,
		Title:        firstNonEmpty(manifest.Title, id),
		TargetBranch: manifest.Repo.TargetBranch,
		TotalTasks:   len(tasks),
		DoneTasks:    countDone(tasks),
		WaveCount:    len(waves),
		SourceFile:   "manifest.json",
	}
	summary.ProgressPct = progressPct(summary.DoneTasks, summary.TotalTasks)
	return summary
}

func summaryFromMarkdown(path, id string) PlanSummary {
	content, err := os.ReadFile(path)
	if err != nil {
		return PlanSummary{ID: id, Title: id, SourceFile: id + ".md"}
	}
	waves, tasks := parseMarkdown(string(content))
	title, branch := parseHeader(string(content))
	return PlanSummary{
		ID:           id,
		Title:        firstNonEmpty(title, id),
		TargetBranch: branch,
		TotalTasks:   len(tasks),
		DoneTasks:    countDone(tasks),
		WaveCount:    len(waves),
		ProgressPct:  progressPct(countDone(tasks), len(tasks)),
		SourceFile:   id + ".md",
	}
}

func loadBundle(dir string) (manifestFile, []WaveDetail, []TaskItem) {
	var manifest manifestFile
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &manifest)
	}
	waves := make([]WaveDetail, 0, len(manifest.Waves))
	tasks := make([]TaskItem, 0)
	for _, ref := range manifest.Waves {
		wave, waveTasks := loadWaveFile(dir, ref)
		waves = append(waves, wave)
		tasks = append(tasks, waveTasks...)
	}
	if len(waves) == 0 && manifest.PlanID != "" {
		waves = append(waves, WaveDetail{ID: manifest.PlanID, Title: manifest.Title})
	}
	return manifest, waves, tasks
}

func loadWaveFile(dir string, ref manifestWaveRef) (WaveDetail, []TaskItem) {
	waveID := firstNonEmpty(ref.ID, "W0")
	wave := WaveDetail{
		ID:        waveID,
		Title:     firstNonEmpty(ref.Title, waveID),
		DependsOn: ref.DependsOn,
	}
	filePath := resolveWavePath(dir, ref.WaveFile)
	if filePath == "" {
		return wave, nil
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return wave, nil
	}
	var parsed waveFile
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return wave, nil
	}
	tasks := make([]TaskItem, 0, len(parsed.Tasks))
	for _, task := range parsed.Tasks {
		tasks = append(tasks, TaskItem{
			Code:       task.Code,
			Title:      task.Title,
			Status:     firstNonEmpty(task.Lifecycle.Status, "backlog"),
			WaveID:     firstNonEmpty(parsed.WaveID, waveID),
			OwnerFiles: task.Governance.OwnerFiles,
			DependsOn:  task.Graph.DependsOn,
		})
	}
	wave.TaskCount = len(tasks)
	wave.DoneCount = countDone(tasks)
	return wave, tasks
}

func resolveWavePath(dir, waveFile string) string {
	clean := strings.TrimSpace(waveFile)
	if clean == "" || filepath.IsAbs(clean) {
		return ""
	}
	clean = strings.TrimPrefix(clean, "./")
	parent := filepath.Dir(dir)
	grand := filepath.Dir(parent)
	bases := []string{dir, parent, grand, filepath.Dir(grand)}
	for _, base := range bases {
		candidate := filepath.Join(base, filepath.FromSlash(clean))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	localWave := filepath.Join(dir, "waves", filepath.Base(clean))
	if _, err := os.Stat(localWave); err == nil {
		return localWave
	}
	return ""
}

func countDone(tasks []TaskItem) int {
	done := 0
	for _, task := range tasks {
		if task.Status == "done" || task.Status == "skipped" {
			done++
		}
	}
	return done
}

func progressPct(done, total int) int {
	if total == 0 {
		return 0
	}
	return done * 100 / total
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
