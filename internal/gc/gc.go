package gc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
	"github.com/TangYani2024/bswarm/internal/worktree"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type TreeCleanReport struct {
	TreeID          string   `json:"tree_id"`
	OrphanWorktrees []string `json:"orphan_worktrees,omitempty"`
	RemovedCount    int      `json:"removed_count"`
}

type NativeSwarmEntry struct {
	Product          string `json:"product"`
	CoordinatorConv  string `json:"coordinator_conv"`
	Title            string `json:"title"`
	BoardBytes       int64  `json:"board_bytes"`
	TotalMembers     int    `json:"total_members"`
	KilledMembers    int    `json:"killed_members"`
	ReclaimableDirs  int    `json:"reclaimable_dirs"`
	ReclaimablePBs   int    `json:"reclaimable_pbs"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	RemovedBytes     int64  `json:"removed_bytes,omitempty"`
}

type NativeAuditReport struct {
	Applied               bool               `json:"applied"`
	OlderThan             string             `json:"older_than"`
	Swarms                []NativeSwarmEntry `json:"swarms"`
	TotalKilledDirs       int                `json:"total_killed_dirs"`
	TotalKilledPBs        int                `json:"total_killed_pbs"`
	TotalReclaimableBytes int64              `json:"total_reclaimable_bytes"`
	TotalRemovedBytes     int64              `json:"total_removed_bytes"`
}

// CleanOrphanWorktrees removes worktree directories under PLUGIN_DATA/worktrees that are
// no longer referenced by any active node in active.json.
func CleanOrphanWorktrees(st *store.Store, treeID string, apply bool) ([]TreeCleanReport, error) {
	var ids []string
	if treeID != "" {
		ids = []string{treeID}
	} else {
		var err error
		ids, err = st.ListTrees()
		if err != nil {
			return nil, err
		}
	}
	var reports []TreeCleanReport
	for _, id := range ids {
		t, err := st.LoadTree(id)
		if err != nil {
			continue
		}
		activeWTs := map[string]bool{}
		for _, n := range t.Nodes {
			if n.WorkspaceMode == store.WorkspaceWorktree && n.WorkspacePath != "" {
				activeWTs[pathKey(n.WorkspacePath)] = true
			}
		}
		wtRoot := st.WorktreesRoot(id)
		entries, err := os.ReadDir(wtRoot)
		if err != nil {
			continue
		}
		rep := TreeCleanReport{TreeID: id}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			full := filepath.Clean(filepath.Join(wtRoot, e.Name()))
			if activeWTs[pathKey(full)] {
				continue
			}
			rep.OrphanWorktrees = append(rep.OrphanWorktrees, full)
			if apply {
				_ = worktree.Remove(t.RepoRoot, full, "", false)
				if _, serr := os.Stat(full); os.IsNotExist(serr) {
					rep.RemovedCount++
				}
			}
		}
		if len(rep.OrphanWorktrees) > 0 {
			reports = append(reports, rep)
		}
	}
	return reports, nil
}

func pathKey(p string) string {
	c := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(c)
	}
	return c
}

// DefaultAppDataDirs returns existing Antigravity product directories under ~/.gemini (plus ANTIGRAVITY_APP_DATA_DIR).
func DefaultAppDataDirs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		k := pathKey(d)
		if seen[k] {
			return
		}
		if fi, err := os.Stat(filepath.Join(d, "brain")); err == nil && fi.IsDir() {
			seen[k] = true
			out = append(out, d)
		}
	}
	add(strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR")))
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"antigravity", "antigravity-cli", "antigravity-ide"} {
			add(filepath.Join(home, ".gemini", name))
		}
	}
	return out
}

// AuditNativeSwarms scans Antigravity native swarm directories for leftover `killed` worker
// brain directories and conversations/*.pb files. Strictly read-only unless apply == true.
func AuditNativeSwarms(appDataDirs []string, olderThan time.Duration, apply bool) (*NativeAuditReport, error) {
	if olderThan < 0 {
		return nil, fmt.Errorf("older-than must be >= 0, got %s", olderThan)
	}
	if len(appDataDirs) == 0 {
		appDataDirs = DefaultAppDataDirs()
	}
	cutoff := time.Now().Add(-olderThan)
	rep := &NativeAuditReport{
		Applied:   apply,
		OlderThan: olderThan.String(),
	}

	for _, appData := range appDataDirs {
		brainRoot := filepath.Join(appData, "brain")
		convRoot := filepath.Join(appData, "conversations")
		product := filepath.Base(appData)
		entries, err := os.ReadDir(brainRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !uuidRe.MatchString(e.Name()) {
				continue
			}
			swarmDir := filepath.Join(brainRoot, e.Name(), ".system_generated", "swarm")
			membersPath := filepath.Join(swarmDir, "members.json")
			raw, err := os.ReadFile(membersPath)
			if err != nil {
				continue
			}
			var roster struct {
				Title   string `json:"title"`
				Members map[string]struct {
					ConversationID string `json:"conversation_id"`
					Role           string `json:"role"`
					Killed         bool   `json:"killed"`
				} `json:"members"`
			}
			if err := json.Unmarshal(raw, &roster); err != nil {
				continue
			}
			entry := NativeSwarmEntry{
				Product:         product,
				CoordinatorConv: e.Name(),
				Title:           roster.Title,
				TotalMembers:    len(roster.Members),
			}
			if bfi, err := os.Stat(filepath.Join(swarmDir, "board.jsonl")); err == nil {
				entry.BoardBytes = bfi.Size()
			}

			for _, m := range roster.Members {
				if !m.Killed || m.Role == "coordinator" {
					continue
				}
				entry.KilledMembers++
				cid := strings.TrimSpace(m.ConversationID)
				// Strict safety whitelist: must be a valid UUID and never equal to coordinator.
				if !uuidRe.MatchString(cid) || cid == e.Name() {
					continue
				}

				wBrain := filepath.Join(brainRoot, cid)
				if wfi, err := os.Stat(wBrain); err == nil && wfi.IsDir() {
					latestMtime := wfi.ModTime()
					if trfi, terr := os.Stat(filepath.Join(wBrain, ".system_generated", "logs", "transcript.jsonl")); terr == nil && trfi.ModTime().After(latestMtime) {
						latestMtime = trfi.ModTime()
					}
					if olderThan == 0 || latestMtime.Before(cutoff) {
						sz := dirSize(wBrain)
						entry.ReclaimableDirs++
						entry.ReclaimableBytes += sz
						rep.TotalKilledDirs++
						rep.TotalReclaimableBytes += sz
						if apply {
							if err := os.RemoveAll(wBrain); err == nil {
								entry.RemovedBytes += sz
								rep.TotalRemovedBytes += sz
							}
						}
					}
				}

				wPB := filepath.Join(convRoot, cid+".pb")
				if pfi, err := os.Stat(wPB); err == nil && !pfi.IsDir() {
					if olderThan <= 0 || pfi.ModTime().Before(cutoff) {
						sz := pfi.Size()
						entry.ReclaimablePBs++
						entry.ReclaimableBytes += sz
						rep.TotalKilledPBs++
						rep.TotalReclaimableBytes += sz
						if apply {
							if err := os.Remove(wPB); err == nil {
								entry.RemovedBytes += sz
								rep.TotalRemovedBytes += sz
							}
						}
					}
				}
			}
			rep.Swarms = append(rep.Swarms, entry)
		}
	}

	sort.Slice(rep.Swarms, func(i, j int) bool {
		return rep.Swarms[i].ReclaimableBytes > rep.Swarms[j].ReclaimableBytes
	})
	return rep, nil
}

func FormatNativeAudit(rep *NativeAuditReport) string {
	var b strings.Builder
	mode := "DRY-RUN (read-only)"
	if rep.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintf(&b, "Native Swarm Zombie Audit [%s | older-than=%s]\n", mode, rep.OlderThan)
	fmt.Fprintf(&b, "Found %d swarm(s) | killed brain dirs=%d | killed .pb=%d | reclaimable=%.1f MB",
		len(rep.Swarms), rep.TotalKilledDirs, rep.TotalKilledPBs, float64(rep.TotalReclaimableBytes)/(1024*1024))
	if rep.Applied {
		fmt.Fprintf(&b, " | removed=%.1f MB", float64(rep.TotalRemovedBytes)/(1024*1024))
	}
	b.WriteByte('\n')
	for _, s := range rep.Swarms {
		title, _ := store.TruncateRunes(s.Title, 52)
		fmt.Fprintf(&b, "  [%s] %s | members=%d (killed=%d) | board=%.1f KB | zombies=%.1f MB | %s\n",
			s.Product, s.CoordinatorConv[:8], s.TotalMembers, s.KilledMembers,
			float64(s.BoardBytes)/1024, float64(s.ReclaimableBytes)/(1024*1024), title)
	}
	if !rep.Applied && rep.TotalReclaimableBytes > 0 {
		b.WriteString("Tip: re-run with `--native-audit --apply --older-than 7d` to reclaim killed worker zombies.\n")
	}
	return store.TruncateBytes(b.String(), store.MaxStdoutBytes, "\n...(truncated)\n")
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
