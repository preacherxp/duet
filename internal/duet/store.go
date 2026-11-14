package duet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var validTaskID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func taskDir(commonDir, id string) string {
	return filepath.Join(commonDir, "duet", "tasks", id)
}

func statePath(commonDir, id string) string {
	return filepath.Join(taskDir(commonDir, id), "state.json")
}

func saveTask(task *Task) error {
	dir := task.taskDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create task state directory: %w", err)
	}
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task state: %w", err)
	}
	temporary, err := os.CreateTemp(dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("create temporary task state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary task state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary task state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary task state: %w", err)
	}
	if err := os.Rename(temporaryName, statePath(task.GitCommonDir, task.ID)); err != nil {
		return fmt.Errorf("replace task state: %w", err)
	}
	return nil
}

func loadTask(commonDir, id string) (*Task, error) {
	if !validTaskID.MatchString(id) {
		return nil, fmt.Errorf("invalid task ID %q", id)
	}
	data, err := os.ReadFile(statePath(commonDir, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("task %q does not exist", id)
		}
		return nil, fmt.Errorf("read task state: %w", err)
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("decode task state: %w", err)
	}
	if task.Version != StateVersion {
		return nil, fmt.Errorf("task %q uses unsupported state version %d", id, task.Version)
	}
	return &task, nil
}

func listTasks(commonDir string) ([]*Task, error) {
	root := filepath.Join(commonDir, "duet", "tasks")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var tasks []*Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		task, err := loadTask(commonDir, entry.Name())
		if err != nil {
			continue
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	return tasks, nil
}
