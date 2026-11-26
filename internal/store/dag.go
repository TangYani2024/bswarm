package store

import (
	"fmt"
	"sort"
)

// ValidateNewTaskDeps checks that every dependency exists (active non-failed/canceled or
// explicitly recorded in CompletedTaskIDs) and that adding taskID -> deps does not create a cycle.
func ValidateNewTaskDeps(t *Tree, taskID string, deps []string) error {
	for _, depID := range deps {
		if depID == taskID {
			return fmt.Errorf("cyclic task dependency: %s depends on itself", taskID)
		}
		if t.CompletedTaskIDs[depID] {
			continue
		}
		tk, ok := t.Tasks[depID]
		if !ok {
			return fmt.Errorf("unknown dependency task %q (not found in active or completed tasks)", depID)
		}
		if tk.State == TaskFailed || tk.State == TaskCanceled {
			return fmt.Errorf("dependency task %q is already %s; cannot depend on it", depID, tk.State)
		}
	}

	// Detect cycle via DFS on active tasks + prospective edge.
	adj := make(map[string][]string, len(t.Tasks)+1)
	for id, tk := range t.Tasks {
		adj[id] = tk.DependsOn
	}
	adj[taskID] = deps

	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(adj))
	var dfs func(u string) error
	dfs = func(u string) error {
		state[u] = visiting
		for _, v := range adj[u] {
			if v == u {
				return fmt.Errorf("cyclic task dependency: %s depends on itself", u)
			}
			if state[v] == visiting {
				return fmt.Errorf("cyclic task dependency involving %s -> %s", u, v)
			}
			if state[v] == unvisited {
				if _, exists := adj[v]; exists {
					if err := dfs(v); err != nil {
						return err
					}
				}
			}
		}
		state[u] = visited
		return nil
	}
	return dfs(taskID)
}

// DependenciesSatisfied reports whether every task in deps is explicitly completed
// (either in t.CompletedTaskIDs or active in t.Tasks with State==TaskDone).
// Absent, failed, or canceled tasks are NEVER treated as satisfied.
func DependenciesSatisfied(t *Tree, deps []string) bool {
	for _, depID := range deps {
		if t.CompletedTaskIDs[depID] {
			continue
		}
		tk, ok := t.Tasks[depID]
		if !ok || tk.State != TaskDone {
			return false
		}
	}
	return true
}

// FailedDependency returns the first dependency in deps that failed, was canceled, or vanished
// without ever being recorded as completed.
func FailedDependency(t *Tree, deps []string) (string, string, bool) {
	for _, depID := range deps {
		if t.CompletedTaskIDs[depID] {
			continue
		}
		tk, ok := t.Tasks[depID]
		if !ok {
			return depID, "canceled/missing", true
		}
		if tk.State == TaskFailed || tk.State == TaskCanceled {
			return depID, string(tk.State), true
		}
	}
	return "", "", false
}

// ReadyQueuedTasks returns queued tasks whose dependencies are all satisfied, sorted by ID.
func ReadyQueuedTasks(t *Tree) []*Task {
	var ready []*Task
	for _, tk := range t.Tasks {
		if tk.State == TaskQueued && DependenciesSatisfied(t, tk.DependsOn) {
			ready = append(ready, tk)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

// HasActiveChildren reports whether nodeID currently has any active (working or queued) child.
func HasActiveChildren(t *Tree, nodeID string) bool {
	for _, n := range t.Nodes {
		if n.ParentID == nodeID && (n.State == NodeWorking || n.State == NodeQueued) {
			return true
		}
	}
	return false
}

// Descendants returns all active descendant node IDs of rootNodeID (post-order: leaves first).
func Descendants(t *Tree, rootNodeID string) []string {
	children := make(map[string][]string)
	for id, n := range t.Nodes {
		if n.ParentID != "" {
			children[n.ParentID] = append(children[n.ParentID], id)
		}
	}
	for p := range children {
		sort.Strings(children[p])
	}
	var out []string
	var walk func(id string)
	walk = func(id string) {
		for _, ch := range children[id] {
			walk(ch)
			out = append(out, ch)
		}
	}
	walk(rootNodeID)
	return out
}

// SubtreeIDs returns rootNodeID and all its active descendants (pre-order).
func SubtreeIDs(t *Tree, rootNodeID string) []string {
	children := make(map[string][]string)
	for id, n := range t.Nodes {
		if n.ParentID != "" {
			children[n.ParentID] = append(children[n.ParentID], id)
		}
	}
	for p := range children {
		sort.Strings(children[p])
	}
	var out []string
	var walk func(id string)
	walk = func(id string) {
		out = append(out, id)
		for _, ch := range children[id] {
			walk(ch)
		}
	}
	if _, ok := t.Nodes[rootNodeID]; ok {
		walk(rootNodeID)
	}
	return out
}
