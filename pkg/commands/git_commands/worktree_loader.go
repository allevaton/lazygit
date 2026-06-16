package git_commands

import (
	"cmp"
	iofs "io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/go-errors/errors"
	"github.com/jesseduffield/lazygit/pkg/commands/models"
	"github.com/jesseduffield/lazygit/pkg/utils"
	"github.com/samber/lo"
	"github.com/spf13/afero"
)

type WorktreeLoader struct {
	*GitCommon
}

func NewWorktreeLoader(gitCommon *GitCommon) *WorktreeLoader {
	return &WorktreeLoader{GitCommon: gitCommon}
}

func (self *WorktreeLoader) GetWorktrees() ([]*models.Worktree, error) {
	currentRepoPath := self.repoPaths.RepoPath()
	worktreePath := self.repoPaths.WorktreePath()

	cmdArgs := NewGitCmd("worktree").Arg("list", "--porcelain").ToArgv()
	worktreesOutput, err := self.cmd.New(cmdArgs).DontLog().RunWithOutput()
	if err != nil {
		return nil, err
	}

	splitLines := strings.Split(
		utils.NormalizeLinefeeds(worktreesOutput), "\n",
	)

	var worktrees []*models.Worktree
	var current *models.Worktree
	for _, splitLine := range splitLines {
		// worktrees are defined over multiple lines and are separated by blank lines
		// so if we reach a blank line we're done with the current worktree
		if len(splitLine) == 0 && current != nil {
			worktrees = append(worktrees, current)
			current = nil
			continue
		}

		// ignore bare repo (not sure why it's even appearing in this list: it's not a worktree)
		if splitLine == "bare" {
			current = nil
			continue
		}

		if strings.HasPrefix(splitLine, "worktree ") {
			path := strings.SplitN(splitLine, " ", 2)[1]
			isMain := path == currentRepoPath
			isCurrent := path == worktreePath
			isPathMissing := self.pathExists(path)

			current = &models.Worktree{
				IsMain:        isMain,
				IsCurrent:     isCurrent,
				IsPathMissing: isPathMissing,
				Path:          path,
				// we defer populating GitDir until a loop below so that
				// we can parallelize the calls to git rev-parse
				GitDir: "",
			}
		} else if strings.HasPrefix(splitLine, "HEAD ") {
			current.Head = strings.SplitN(splitLine, " ", 2)[1]
		} else if strings.HasPrefix(splitLine, "branch ") {
			branch := strings.SplitN(splitLine, " ", 2)[1]
			current.Branch = strings.TrimPrefix(branch, "refs/heads/")
		}
	}

	wg := sync.WaitGroup{}
	wg.Add(len(worktrees))
	for _, worktree := range worktrees {
		go utils.Safe(func() {
			defer wg.Done()

			if worktree.IsPathMissing {
				return
			}
			gitDir, err := callGitRevParseWithDir(self.cmd, worktree.Path, "--absolute-git-dir")
			if err != nil {
				self.Log.Warnf("Could not find git dir for worktree %s: %v", worktree.Path, err)
				return
			}

			worktree.GitDir = gitDir
		})
	}
	wg.Wait()

	names := getUniqueNamesFromPaths(lo.Map(worktrees, func(worktree *models.Worktree, _ int) string {
		return worktree.Path
	}))

	for index, worktree := range worktrees {
		worktree.Name = names[index]
	}

	self.sortWorktrees(worktrees)

	// move current worktree to the top
	for i, worktree := range worktrees {
		if worktree.IsCurrent {
			worktrees = append(worktrees[:i], worktrees[i+1:]...)
			worktrees = append([]*models.Worktree{worktree}, worktrees...)
			break
		}
	}

	// Some worktrees are on a branch but are mid-rebase, and in those cases,
	// `git worktree list` will not show the branch name. We can get the branch
	// name from the `rebase-merge/head-name` file (if it exists) in the folder
	// for the worktree in the parent repo's .git/worktrees folder.
	for _, worktree := range worktrees {
		// No point checking if we already have a branch name
		if worktree.Branch != "" {
			continue
		}

		// If we couldn't find the git directory, we can't find the branch name
		if worktree.GitDir == "" {
			continue
		}

		rebasedBranch, ok := self.rebasedBranch(worktree)
		if ok {
			worktree.Branch = rebasedBranch
			continue
		}

		bisectedBranch, ok := self.bisectedBranch(worktree)
		if ok {
			worktree.Branch = bisectedBranch
			continue
		}
	}

	return worktrees, nil
}

// sortWorktrees orders worktrees in place according to git.worktreeSortOrder.
// The current worktree is moved to the top separately by the caller, so it is
// not special-cased here.
func (self *WorktreeLoader) sortWorktrees(worktrees []*models.Worktree) {
	switch self.UserConfig().Git.WorktreeSortOrder {
	case "alphabetical":
		slices.SortFunc(worktrees, func(a, b *models.Worktree) int {
			return strings.Compare(a.Name, b.Name)
		})
	case "date":
		// A worktree's HEAD is the tip of its checked-out branch, so the HEAD's
		// commit date is that branch's "last updated at". Using HEAD rather than
		// the branch name also gives a sensible value for detached worktrees.
		commitDates := self.getHeadCommitDates(worktrees)
		slices.SortStableFunc(worktrees, func(a, b *models.Worktree) int {
			// Newest first. Worktrees with no resolvable date (0) sort last.
			return cmp.Compare(commitDates[b.Head], commitDates[a.Head])
		})
	}
}

// getHeadCommitDates returns a map from a worktree HEAD hash to its committer
// date (unix seconds). All HEADs are resolved in a single git call. Any HEAD
// that can't be resolved is simply absent from the map (treated as 0 by
// callers), so an error here degrades to leaving worktrees in git's order.
func (self *WorktreeLoader) getHeadCommitDates(worktrees []*models.Worktree) map[string]int64 {
	hashes := lo.FilterMap(worktrees, func(worktree *models.Worktree, _ int) (string, bool) {
		return worktree.Head, worktree.Head != ""
	})
	if len(hashes) == 0 {
		return nil
	}

	cmdArgs := NewGitCmd("show").
		Arg("--no-patch", "--format=%H %ct").
		Arg(hashes...).
		ToArgv()

	output, err := self.cmd.New(cmdArgs).DontLog().RunWithOutput()
	if err != nil {
		self.Log.Warnf("Could not get commit dates for worktree sorting: %v", err)
		return nil
	}

	result := make(map[string]int64, len(hashes))
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		hash, dateStr, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		date, err := strconv.ParseInt(dateStr, 10, 64)
		if err != nil {
			continue
		}
		result[hash] = date
	}

	return result
}

func (self *WorktreeLoader) pathExists(path string) bool {
	if _, err := self.Fs.Stat(path); err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return true
		}
		self.Log.Errorf("failed to check if worktree path `%s` exists\n%v", path, err)
		return false
	}
	return false
}

func (self *WorktreeLoader) rebasedBranch(worktree *models.Worktree) (string, bool) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if bytesContent, err := afero.ReadFile(self.Fs, filepath.Join(worktree.GitDir, dir, "head-name")); err == nil {
			headName := strings.TrimSpace(string(bytesContent))
			shortHeadName := strings.TrimPrefix(headName, "refs/heads/")
			return shortHeadName, true
		}
	}

	return "", false
}

func (self *WorktreeLoader) bisectedBranch(worktree *models.Worktree) (string, bool) {
	bisectStartPath := filepath.Join(worktree.GitDir, "BISECT_START")
	startContent, err := afero.ReadFile(self.Fs, bisectStartPath)
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(string(startContent)), true
}

type pathWithIndexT struct {
	path  string
	index int
}

type nameWithIndexT struct {
	name  string
	index int
}

func getUniqueNamesFromPaths(paths []string) []string {
	pathsWithIndex := lo.Map(paths, func(path string, index int) pathWithIndexT {
		return pathWithIndexT{path, index}
	})

	namesWithIndex := getUniqueNamesFromPathsAux(pathsWithIndex, 0)

	// now sort based on index
	result := make([]string, len(namesWithIndex))
	for _, nameWithIndex := range namesWithIndex {
		result[nameWithIndex.index] = nameWithIndex.name
	}

	return result
}

func getUniqueNamesFromPathsAux(paths []pathWithIndexT, depth int) []nameWithIndexT {
	// If we have no paths, return an empty array
	if len(paths) == 0 {
		return []nameWithIndexT{}
	}

	// If we have only one path, return the last segment of the path
	if len(paths) == 1 {
		path := paths[0]
		return []nameWithIndexT{{index: path.index, name: sliceAtDepth(path.path, depth)}}
	}

	// group the paths by their value at the specified depth
	groups := make(map[string][]pathWithIndexT)
	for _, path := range paths {
		value := valueAtDepth(path.path, depth)
		groups[value] = append(groups[value], path)
	}

	result := []nameWithIndexT{}
	for _, group := range groups {
		if len(group) == 1 {
			path := group[0]
			result = append(result, nameWithIndexT{index: path.index, name: sliceAtDepth(path.path, depth)})
		} else {
			result = append(result, getUniqueNamesFromPathsAux(group, depth+1)...)
		}
	}

	return result
}

// if the path is /a/b/c/d, and the depth is 0, the value is 'd'. If the depth is 1, the value is 'c', etc
func valueAtDepth(path string, depth int) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	// Split the path into segments
	segments := strings.Split(path, "/")

	// Get the length of segments
	length := len(segments)

	// If the depth is greater than the length of segments, return an empty string
	if depth >= length {
		return ""
	}

	// Return the segment at the specified depth from the end of the path
	return segments[length-1-depth]
}

// if the path is /a/b/c/d, and the depth is 0, the value is 'd'. If the depth is 1, the value is 'b/c', etc
func sliceAtDepth(path string, depth int) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	// Split the path into segments
	segments := strings.Split(path, "/")

	// Get the length of segments
	length := len(segments)

	// If the depth is greater than or equal to the length of segments, return an empty string
	if depth >= length {
		return ""
	}

	// Join the segments from the specified depth till end of the path
	return strings.Join(segments[length-1-depth:], "/")
}
