package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Access is serialized by Store.writer. Filesystem probes happen once per path.
type projectResolver struct {
	cache  map[string]string
	repos  map[string]map[string]bool
	absent map[string]bool
}

func newProjectResolver() *projectResolver {
	return &projectResolver{cache: map[string]string{}, repos: map[string]map[string]bool{}, absent: map[string]bool{}}
}
func cleanProject(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Clean(p)
}
func (r *projectResolver) probe(raw string) string {
	p := cleanProject(raw)
	if p == "" {
		return ""
	}
	if v, ok := r.cache[p]; ok {
		return v
	}
	canonical := p
	if _, err := os.Stat(p); os.IsNotExist(err) {
		r.absent[p] = true
	}
	if filepath.IsAbs(p) {
		for dir := p; ; dir = filepath.Dir(dir) {
			git := filepath.Join(dir, ".git")
			if info, err := os.Stat(git); err == nil {
				canonical = dir
				if !info.IsDir() {
					if data, err := os.ReadFile(git); err == nil && strings.HasPrefix(string(data), "gitdir:") {
						target := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
						if !filepath.IsAbs(target) {
							target = filepath.Join(dir, target)
						}
						target = filepath.Clean(target)
						// Linked worktree administrative dirs live under the main repo's .git.
						if parent := filepath.Dir(target); filepath.Base(parent) == "worktrees" && filepath.Base(filepath.Dir(parent)) == ".git" {
							canonical = filepath.Dir(filepath.Dir(parent))
						}
					}
				}
				name := filepath.Base(canonical)
				if r.repos[name] == nil {
					r.repos[name] = map[string]bool{}
				}
				r.repos[name][canonical] = true
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	r.cache[p] = canonical
	return canonical
}
func (r *projectResolver) resolve(raw string) string {
	p := cleanProject(raw)
	v := r.probe(p)
	if v == p && r.absent[p] {
		// Only collapse an absent path when the basename identifies one known repo.
		if matches := r.repos[filepath.Base(p)]; len(matches) == 1 {
			for candidate := range matches {
				return candidate
			}
		}
	}
	return v
}
func (s *Store) NormalizeProjects(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	return s.normalizeProjects(ctx)
}
func (s *Store) normalizeProjects(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT raw_project,project FROM events GROUP BY raw_project,project UNION SELECT raw_project,project FROM sessions GROUP BY raw_project,project")
	if err != nil {
		return err
	}
	var paths []string
	stored := map[string]map[string]bool{}
	for rows.Next() {
		var p, canonical string
		if err = rows.Scan(&p, &canonical); err != nil {
			rows.Close()
			return err
		}
		if stored[p] == nil {
			stored[p] = map[string]bool{}
			paths = append(paths, p)
		}
		stored[p][canonical] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		s.projects.probe(p)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	for _, p := range paths {
		canonical := s.projects.resolve(p)
		if len(stored[p]) == 1 && stored[p][canonical] {
			continue
		}
		changed = true
		for _, table := range []string{"events", "sessions"} {
			if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET project=? WHERE raw_project=? AND project!=?", canonical, p, canonical); err != nil {
				return err
			}
		}
	}
	err = tx.Commit()
	if err == nil && changed {
		s.notify()
	}
	return err
}
