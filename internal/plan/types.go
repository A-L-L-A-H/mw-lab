package plan

// PlanSummary is the compact card-level view of a discovered plan.
type PlanSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	TargetBranch string `json:"targetBranch"`
	TotalTasks   int    `json:"totalTasks"`
	DoneTasks    int    `json:"doneTasks"`
	WaveCount    int    `json:"waveCount"`
	ProgressPct  int    `json:"progressPct"`
	SourceFile   string `json:"sourceFile"`
}

// PlanDetail is the full view of a plan including its waves and tasks.
type PlanDetail struct {
	PlanSummary
	Waves []WaveDetail `json:"waves"`
	Tasks []TaskItem   `json:"tasks"`
}

// WaveDetail is a compact view of one wave inside a plan.
type WaveDetail struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	DependsOn []string `json:"dependsOn"`
	TaskCount int      `json:"taskCount"`
	DoneCount int      `json:"doneCount"`
}

// TaskItem is one micro-task from a wave or markdown checklist.
type TaskItem struct {
	Code       string   `json:"code"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	WaveID     string   `json:"waveId"`
	OwnerFiles []string `json:"ownerFiles"`
	DependsOn  []string `json:"dependsOn"`
}

type manifestWaveRef struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	WaveFile  string   `json:"wave_file"`
	DependsOn []string `json:"depends_on"`
}

type manifestFile struct {
	PlanID string `json:"plan_id"`
	Title  string `json:"title"`
	Repo   struct {
		TargetBranch string `json:"target_branch"`
	} `json:"repo"`
	Waves []manifestWaveRef `json:"waves"`
}

type waveFile struct {
	WaveID string     `json:"wave_id"`
	Title  string     `json:"title"`
	Tasks  []waveTask `json:"tasks"`
}

type waveTask struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	Lifecycle struct {
		Status string `json:"status"`
	} `json:"lifecycle"`
	Governance struct {
		OwnerFiles []string `json:"owner_files"`
	} `json:"governance"`
	Graph struct {
		DependsOn []string `json:"depends_on"`
	} `json:"graph"`
}
