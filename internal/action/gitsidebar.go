package action

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micro-editor/micro/v2/internal/buffer"
	"github.com/micro-editor/micro/v2/internal/config"
	"github.com/micro-editor/micro/v2/internal/display"
	"github.com/micro-editor/micro/v2/internal/screen"
	"github.com/micro-editor/tcell/v2"
)

type GitFile struct {
	Path   string
	Status string // "A", "M", "D", "R", "C", "?" etc.
}

type GitCommit struct {
	Hash    string
	Subject string
}

type RenderLine struct {
	text     string
	style    tcell.Style
	isSel    bool
	itemType string // "commit", "pull", "push", "staged", "change", "sep", "staged_title", "changes_title", "commit_history"
	path     string // path of file or commit hash
}

type GitSidebarPane struct {
	view       *display.View
	is_active  bool
	pane_id    uint64
	parent_tab *Tab

	root_dir    string
	scroll_y    int
	selected_y  int

	commitInput  string
	commitCursor int

	stagedFiles   []GitFile
	changedFiles  []GitFile
	commits       []GitCommit
	currentBranch string
	hasRemote     bool
	canPush       bool
	canPull       bool
	needingPush   map[string]bool

	renderLines []RenderLine

	branchSearchMode    bool
	branchSearchInput   string
	branchSearchCursor  int
	branchSearchResults []string
	allBranches         []string

	lastRefresh time.Time
	closed      bool
	mutex       sync.Mutex

	targetSelType string
	targetSelPath string
}

func NewGitSidebarPane(dir string, tab *Tab) *GitSidebarPane {
	s := &GitSidebarPane{
		view:        &display.View{},
		parent_tab:  tab,
		root_dir:    dir,
		needingPush: make(map[string]bool),
	}
	s.refreshGitStatus(true)
	s.rebuildRenderLines()
	return s
}

func (s *GitSidebarPane) ID() uint64 {
	return s.pane_id
}

func (s *GitSidebarPane) SetID(i uint64) {
	s.pane_id = i
}

func (s *GitSidebarPane) Name() string {
	return "Git"
}

func (s *GitSidebarPane) Close() {
	s.Quit()
}

func (s *GitSidebarPane) SetTab(t *Tab) {
	s.parent_tab = t
}

func (s *GitSidebarPane) Tab() *Tab {
	return s.parent_tab
}

func (s *GitSidebarPane) Clear() {
	for y := 0; y < s.view.Height; y++ {
		for x := 0; x < s.view.Width; x++ {
			screen.SetContent(s.view.X+x, s.view.Y+y, ' ', nil, config.DefStyle)
		}
	}
}

func (s *GitSidebarPane) Relocate() bool {
	return false
}

func (s *GitSidebarPane) GetView() *display.View {
	return s.view
}

func (s *GitSidebarPane) SetView(v *display.View) {
	s.view = v
}

func (s *GitSidebarPane) LocFromVisual(vloc buffer.Loc) buffer.Loc {
	return vloc
}

func (s *GitSidebarPane) Resize(w, h int) {
	s.view.Width = w
	s.view.Height = h
}

func (s *GitSidebarPane) SetActive(b bool) {
	s.is_active = b
}

func (s *GitSidebarPane) IsActive() bool {
	return s.is_active
}

func (s *GitSidebarPane) HandleCommand(cmd string) {
}

func (s *GitSidebarPane) isGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = s.root_dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func (s *GitSidebarPane) refreshGitStatus(force bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.root_dir == "" {
		return
	}

	if !force && time.Since(s.lastRefresh) < 2*time.Second {
		return
	}
	s.lastRefresh = time.Now()

	if !s.isGitRepo() {
		s.currentBranch = ""
		s.stagedFiles = nil
		s.changedFiles = nil
		s.commits = nil
		s.hasRemote = false
		return
	}

	// 1. Current Branch
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = s.root_dir
	out, err := cmd.Output()
	if err == nil {
		s.currentBranch = strings.TrimSpace(string(out))
		if s.currentBranch == "" {
			cmdHEAD := exec.Command("git", "rev-parse", "--short", "HEAD")
			cmdHEAD.Dir = s.root_dir
			outHEAD, errHEAD := cmdHEAD.Output()
			if errHEAD == nil {
				s.currentBranch = "HEAD (" + strings.TrimSpace(string(outHEAD)) + ")"
			}
		}
	}

	// 2. Remotes check
	cmdRemote := exec.Command("git", "remote")
	cmdRemote.Dir = s.root_dir
	outRemote, errRemote := cmdRemote.Output()
	s.hasRemote = errRemote == nil && len(strings.TrimSpace(string(outRemote))) > 0

	// 3. Staged & Unstaged files
	cmdStatus := exec.Command("git", "status", "--porcelain")
	cmdStatus.Dir = s.root_dir
	outStatus, errStatus := cmdStatus.Output()
	if errStatus == nil {
		s.stagedFiles = []GitFile{}
		s.changedFiles = []GitFile{}
		lines := strings.Split(string(outStatus), "\n")
		for _, line := range lines {
			if len(line) < 4 {
				continue
			}
			x := line[0]
			y := line[1]
			path := line[3:]

			cleanPath := path
			if strings.Contains(cleanPath, " -> ") {
				parts := strings.Split(cleanPath, " -> ")
				cleanPath = parts[len(parts)-1]
			}

			// Staged list
			if x == 'M' || x == 'A' || x == 'D' || x == 'R' || x == 'C' {
				statusChar := string(x)
				s.stagedFiles = append(s.stagedFiles, GitFile{
					Path:   cleanPath,
					Status: statusChar,
				})
			}

			// Changes list
			if y == 'M' || y == 'D' || (x == '?' && y == '?') {
				statusChar := string(y)
				if x == '?' && y == '?' {
					statusChar = "?"
				}
				s.changedFiles = append(s.changedFiles, GitFile{
					Path:   cleanPath,
					Status: statusChar,
				})
			}
		}
	}

	// 4. Commits (last 50)
	cmdCommits := exec.Command("git", "log", "-n", "50", "--pretty=format:%H %s")
	cmdCommits.Dir = s.root_dir
	outCommits, errCommits := cmdCommits.Output()
	if errCommits == nil {
		s.commits = []GitCommit{}
		lines := strings.Split(string(outCommits), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 2 {
				s.commits = append(s.commits, GitCommit{
					Hash:    parts[0],
					Subject: parts[1],
				})
			} else if len(parts) == 1 {
				s.commits = append(s.commits, GitCommit{
					Hash:    parts[0],
					Subject: "",
				})
			}
		}
	}

	// 5. Needing push map
	s.needingPush = make(map[string]bool)
	cmdUpstream := exec.Command("git", "rev-parse", "--abbrev-ref", "@{u}")
	cmdUpstream.Dir = s.root_dir
	outUpstream, errUpstream := cmdUpstream.Output()
	if errUpstream == nil && len(strings.TrimSpace(string(outUpstream))) > 0 {
		upstream := strings.TrimSpace(string(outUpstream))
		cmdNP := exec.Command("git", "log", upstream+"..HEAD", "--format=%H")
		cmdNP.Dir = s.root_dir
		outNP, errNP := cmdNP.Output()
		if errNP == nil {
			for _, line := range strings.Split(string(outNP), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					s.needingPush[line] = true
				}
			}
		}
	} else if s.hasRemote {
		for _, c := range s.commits {
			s.needingPush[c.Hash] = true
		}
	}

	// 6. canPush / canPull checks
	s.canPush = false
	s.canPull = false
	if s.hasRemote {
		cmdRP := exec.Command("git", "rev-list", "--left-right", "--count", "HEAD...@{u}")
		cmdRP.Dir = s.root_dir
		outRP, errRP := cmdRP.Output()
		if errRP == nil {
			fields := strings.Fields(string(outRP))
			if len(fields) >= 2 {
				ahead, errAhead := strconv.Atoi(fields[0])
				behind, errBehind := strconv.Atoi(fields[1])
				if errAhead == nil && ahead > 0 {
					s.canPush = true
				}
				if errBehind == nil && behind > 0 {
					s.canPull = true
				}
			}
		} else {
			if len(s.needingPush) > 0 {
				s.canPush = true
			}
		}
	}
}

func getGitStyle(status string) tcell.Style {
	var colName string
	switch status {
	case "A", "C", "?":
		colName = "green"
	case "M", "R":
		colName = "blue"
	case "D":
		colName = "red"
	default:
		colName = "white"
	}
	col, _ := config.StringToColor(colName)
	return config.DefStyle.Foreground(col)
}

func getCommitStyle(hash string, needingPush map[string]bool) tcell.Style {
	colName := "magenta" // purple
	if needingPush[hash] {
		colName = "blue"
	}
	col, _ := config.StringToColor(colName)
	return config.DefStyle.Foreground(col)
}

func (s *GitSidebarPane) rebuildRenderLines() {
	var selType string
	var selPath string
	if s.targetSelType != "" {
		selType = s.targetSelType
		selPath = s.targetSelPath
		s.targetSelType = ""
		s.targetSelPath = ""
	} else if s.selected_y >= 0 && s.selected_y < len(s.renderLines) {
		selType = s.renderLines[s.selected_y].itemType
		selPath = s.renderLines[s.selected_y].path
	}

	s.renderLines = []RenderLine{}

	grey_color, _ := config.StringToColor("brightblack")
	grey_style := config.DefStyle.Foreground(grey_color)

	// Note: Workspace directory title and current branch name are drawn as fixed
	// lines at the top of the sidebar. The scrollable region starts below them.

	// A0. Branch name
	if s.isGitRepo() {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     s.currentBranch,
			style:    grey_style,
			isSel:    true,
			itemType: "branch",
		})
	}

	// A. Commit text input
	if s.currentBranch != "" && len(s.stagedFiles) > 0 {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "Commit: " + s.commitInput,
			style:    config.DefStyle,
			isSel:    true,
			itemType: "commit",
		})
		// B. Separation line (empty line)
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "",
			style:    config.DefStyle,
			isSel:    false,
			itemType: "sep",
		})
	}

	// C. Pull/Push buttons
	hasButtons := false
	if s.hasRemote {
		if s.canPull {
			s.renderLines = append(s.renderLines, RenderLine{
				text:     "[ Pull ]",
				style:    config.DefStyle,
				isSel:    true,
				itemType: "pull",
			})
			hasButtons = true
		}
		if s.canPush {
			s.renderLines = append(s.renderLines, RenderLine{
				text:     "[ Push ]",
				style:    config.DefStyle,
				isSel:    true,
				itemType: "push",
			})
			hasButtons = true
		}
	}

	// D. Separation line if any button (empty line)
	if hasButtons {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "",
			style:    config.DefStyle,
			isSel:    false,
			itemType: "sep",
		})
	}

	// E. Staged list
	if len(s.stagedFiles) > 0 {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "Staged",
			style:    grey_style,
			isSel:    false,
			itemType: "staged_title",
		})
		for _, f := range s.stagedFiles {
			s.renderLines = append(s.renderLines, RenderLine{
				text:     "- " + f.Path,
				style:    getGitStyle(f.Status),
				isSel:    true,
				itemType: "staged",
				path:     f.Path,
			})
		}
		// F. Separation line if staged file (empty line)
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "",
			style:    config.DefStyle,
			isSel:    false,
			itemType: "sep",
		})
	}

	// G. Changes list
	if len(s.changedFiles) > 0 {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "Changes",
			style:    grey_style,
			isSel:    false,
			itemType: "changes_title",
		})
		for _, f := range s.changedFiles {
			s.renderLines = append(s.renderLines, RenderLine{
				text:     "+ " + f.Path,
				style:    getGitStyle(f.Status),
				isSel:    true,
				itemType: "change",
				path:     f.Path,
			})
		}
		// H. Separation line if changes (empty line)
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "",
			style:    config.DefStyle,
			isSel:    false,
			itemType: "sep",
		})
	}

	// I. Commits (last 30)
	for _, c := range s.commits {
		s.renderLines = append(s.renderLines, RenderLine{
			text:     "• " + c.Subject,
			style:    getCommitStyle(c.Hash, s.needingPush),
			isSel:    false,
			itemType: "commit_history",
			path:     c.Hash,
		})
	}

	// Restore selection
	newSelIdx := -1
	if selType != "" {
		for i, line := range s.renderLines {
			if line.isSel && line.itemType == selType && line.path == selPath {
				newSelIdx = i
				break
			}
		}
	}
	if newSelIdx != -1 {
		s.selected_y = newSelIdx
	} else {
		s.selected_y = -1
		for i, line := range s.renderLines {
			if line.isSel {
				s.selected_y = i
				if line.itemType != "branch" {
					break
				}
			}
		}
		if s.selected_y == -1 {
			for i, line := range s.renderLines {
				if line.isSel {
					s.selected_y = i
					break
				}
			}
		}
	}
	s.scrollToSelected()
}

func (s *GitSidebarPane) findNextPrevOrNearestFile(currentPath string, currentType string) (string, string) {
	type fileItem struct {
		index    int
		itemType string
		path     string
	}
	var files []fileItem
	currentIdx := -1

	for i, line := range s.renderLines {
		if line.itemType == "staged" || line.itemType == "change" {
			if line.itemType == currentType && line.path == currentPath {
				currentIdx = i
			} else {
				files = append(files, fileItem{
					index:    i,
					itemType: line.itemType,
					path:     line.path,
				})
			}
		}
	}

	if currentIdx == -1 || len(files) == 0 {
		return "", ""
	}

	// Try to find the next file of the SAME type
	var nextSame *fileItem
	for _, f := range files {
		if f.itemType == currentType && f.index > currentIdx {
			if nextSame == nil || f.index < nextSame.index {
				fCopy := f
				nextSame = &fCopy
			}
		}
	}
	if nextSame != nil {
		return nextSame.itemType, nextSame.path
	}

	// Try to find the previous file of the SAME type
	var prevSame *fileItem
	for _, f := range files {
		if f.itemType == currentType && f.index < currentIdx {
			if prevSame == nil || f.index > prevSame.index {
				fCopy := f
				prevSame = &fCopy
			}
		}
	}
	if prevSame != nil {
		return prevSame.itemType, prevSame.path
	}

	// Try to find the nearest file in the OTHER list
	var nearestOther *fileItem
	minDist := -1
	otherType := "staged"
	if currentType == "staged" {
		otherType = "change"
	}
	for _, f := range files {
		if f.itemType == otherType {
			dist := currentIdx - f.index
			if dist < 0 {
				dist = -dist
			}
			if minDist == -1 || dist < minDist {
				minDist = dist
				fCopy := f
				nearestOther = &fCopy
			}
		}
	}
	if nearestOther != nil {
		return nearestOther.itemType, nearestOther.path
	}

	return "", ""
}

func (s *GitSidebarPane) scrollToSelected() {
	if s.selected_y < 0 || len(s.renderLines) == 0 {
		return
	}
	availHeight := s.view.Height - 3
	if availHeight < 1 {
		availHeight = 1
	}
	if s.selected_y < s.scroll_y {
		s.scroll_y = s.selected_y
	} else if s.selected_y >= s.scroll_y+availHeight {
		s.scroll_y = s.selected_y - availHeight + 1
	}
}

func (s *GitSidebarPane) selectNext() {
	if len(s.renderLines) == 0 {
		return
	}
	for i := s.selected_y + 1; i < len(s.renderLines); i++ {
		if s.renderLines[i].isSel {
			s.selected_y = i
			s.scrollToSelected()
			return
		}
	}
}

func (s *GitSidebarPane) selectPrev() {
	if len(s.renderLines) == 0 {
		return
	}
	for i := s.selected_y - 1; i >= 0; i-- {
		if s.renderLines[i].isSel {
			s.selected_y = i
			s.scrollToSelected()
			return
		}
	}
}

func (s *GitSidebarPane) Display() {
	if s.closed {
		return
	}

	if s.branchSearchMode {
		s.Clear()
		grey_color, _ := config.StringToColor("brightblack")
		grey_style := config.DefStyle.Foreground(grey_color)

		title := "BRANCH SEARCH"
		for x := 0; x < s.view.Width; x++ {
			var r rune = ' '
			if x < len(title) {
				r = rune(title[x])
			}
			screen.SetContent(s.view.X+x, s.view.Y, r, nil, grey_style)
		}

		prompt := "> "
		input_str := prompt + s.branchSearchInput
		input_runes := []rune(input_str)
		white_color, _ := config.StringToColor("brightwhite")
		white_style := config.DefStyle.Foreground(white_color)

		for x := 0; x < s.view.Width; x++ {
			var r rune = ' '
			if x < len(input_runes) {
				r = input_runes[x]
			}
			screen.SetContent(s.view.X+x, s.view.Y+1, r, nil, white_style)
		}

		// Draw cursor
		screen.ShowCursor(s.view.X+len(prompt)+s.branchSearchCursor, s.view.Y+1)

		// Line 2: Empty line
		for x := 0; x < s.view.Width; x++ {
			screen.SetContent(s.view.X+x, s.view.Y+2, ' ', nil, config.DefStyle)
		}

		// Scrollable search results starting at Line 3
		for y := 3; y < s.view.Height; y++ {
			index := (y - 3) + s.scroll_y
			if index >= len(s.branchSearchResults) {
				for x := 0; x < s.view.Width; x++ {
					screen.SetContent(s.view.X+x, s.view.Y+y, ' ', nil, config.DefStyle)
				}
				continue
			}

			branchName := s.branchSearchResults[index]
			style := white_style
			if index == s.selected_y {
				style = style.Reverse(true)
			}

			runes := []rune(branchName)
			for x := 0; x < s.view.Width; x++ {
				var r rune = ' '
				if x < len(runes) {
					r = runes[x]
				}
				screen.SetContent(s.view.X+x, s.view.Y+y, r, nil, style)
			}
		}
		return
	}

	s.refreshGitStatus(false)
	s.rebuildRenderLines()

	grey_color, _ := config.StringToColor("brightblack")
	grey_style := config.DefStyle.Foreground(grey_color)

	// Fixed Line 0: Gray workspace directory name (like in file tree)
	abs_dir, err := filepath.Abs(s.root_dir)
	title_str := filepath.Base(s.root_dir)
	if err == nil {
		title_str = filepath.Base(abs_dir)
	}
	title_runes := []rune(title_str)
	for x := 0; x < s.view.Width; x++ {
		var r rune = ' '
		if x < len(title_runes) {
			r = title_runes[x]
		}
		screen.SetContent(s.view.X+x, s.view.Y, r, nil, grey_style)
	}

	// Fixed Line 1: Gray current branch name (keeps gray unless selected)
	branchText := s.currentBranch
	if branchText == "" {
		branchText = "no git repository"
	}
	branchStyle := grey_style
	if s.is_active && s.selected_y >= 0 && s.selected_y < len(s.renderLines) && s.renderLines[s.selected_y].itemType == "branch" {
		branchStyle = grey_style.Reverse(true)
	}
	branch_runes := []rune(branchText)
	for x := 0; x < s.view.Width; x++ {
		var r rune = ' '
		if x < len(branch_runes) {
			r = branch_runes[x]
		}
		screen.SetContent(s.view.X+x, s.view.Y+1, r, nil, branchStyle)
	}

	// Fixed Line 2: Empty line under branch name
	for x := 0; x < s.view.Width; x++ {
		screen.SetContent(s.view.X+x, s.view.Y+2, ' ', nil, config.DefStyle)
	}

	// Scrollable lines starting at Line 3
	for y := 3; y < s.view.Height; y++ {
		index := (y - 3) + s.scroll_y
		actualIndex := index
		if s.isGitRepo() {
			actualIndex = index + 1
		}
		if actualIndex >= len(s.renderLines) {
			for x := 0; x < s.view.Width; x++ {
				screen.SetContent(s.view.X+x, s.view.Y+y, ' ', nil, config.DefStyle)
			}
			continue
		}

		line := s.renderLines[actualIndex]
		style := line.style

		if actualIndex == s.selected_y {
			style = style.Reverse(true)
		}

		runes := []rune(line.text)
		for x := 0; x < s.view.Width; x++ {
			var r rune = ' '
			if x < len(runes) {
				r = runes[x]
			}
			screen.SetContent(s.view.X+x, s.view.Y+y, r, nil, style)
		}

		if s.is_active && actualIndex == s.selected_y && line.itemType == "commit" {
			cursorX := s.view.X + 8 + s.commitCursor
			if cursorX >= s.view.X+s.view.Width {
				cursorX = s.view.X + s.view.Width - 1
			}
			screen.ShowCursor(cursorX, s.view.Y+y)
		}
	}
}

func (s *GitSidebarPane) openFileInWorkspace(path string) {
	fullPath := filepath.Join(s.root_dir, path)
	b, err := buffer.NewBufferFromFile(fullPath, buffer.BTDefault)
	if err == nil {
		active_editor_idx := -1
		for i, p := range s.parent_tab.Panes {
			if _, ok := p.(*BufPane); ok {
				if p.IsActive() {
					active_editor_idx = i
					break
				}
			}
		}
		if active_editor_idx != -1 {
			bp := s.parent_tab.Panes[active_editor_idx].(*BufPane)
			bp.VSplitBuf(b)
		} else {
			var bp *BufPane
			for _, p := range s.parent_tab.Panes {
				if b_pane, ok := p.(*BufPane); ok {
					bp = b_pane
					break
				}
			}
			if bp != nil {
				bp.VSplitBuf(b)
			} else {
				e := NewBufPaneFromBuf(b, s.parent_tab)
				e.splitID = s.parent_tab.GetNode(s.pane_id).VSplit(true)
				s.parent_tab.AddPane(e, 1)
				left_node := s.parent_tab.GetNode(s.ID())
				if left_node != nil {
					left_node.ResizeSplit(64)
				}
				s.parent_tab.Resize()
				s.parent_tab.SetActive(1)
			}
		}
	}
}

func (s *GitSidebarPane) HandleEvent(event tcell.Event) {
	if s.closed {
		return
	}

	if s.branchSearchMode {
		if e, ok := event.(*tcell.EventKey); ok {
			// Check ctrl+enter (ModCtrl) for new branch creation
			if e.Key() == tcell.KeyEnter && e.Modifiers()&tcell.ModCtrl != 0 {
				newBranchName := strings.TrimSpace(s.branchSearchInput)
				if newBranchName != "" {
					InfoBar.YNPrompt("Create and checkout to new branch '"+newBranchName+"'? (y,n)", func(yes, canceled bool) {
						if !canceled && yes {
							cmd := exec.Command("git", "checkout", "-b", newBranchName)
							cmd.Dir = s.root_dir
							if out, err := cmd.CombinedOutput(); err != nil {
								InfoBar.Error("Error: " + strings.TrimSpace(string(out)))
							} else {
								s.branchSearchMode = false
								s.selected_y = 0 // back to branch item
								s.refreshGitStatus(true)
								s.rebuildRenderLines()
							}
						}
					})
				}
				return
			}

			switch e.Key() {
			case tcell.KeyEscape:
				s.branchSearchMode = false
				s.selected_y = 0
				s.rebuildRenderLines()
				return
			case tcell.KeyCtrlW:
				s.Quit()
				return
			case tcell.KeyEnter:
				if s.selected_y >= 0 && s.selected_y < len(s.branchSearchResults) {
					targetBranch := s.branchSearchResults[s.selected_y]
					cmd := exec.Command("git", "checkout", targetBranch)
					cmd.Dir = s.root_dir
					if out, err := cmd.CombinedOutput(); err != nil {
						InfoBar.Error("Error checking out branch: " + strings.TrimSpace(string(out)))
					} else {
						s.branchSearchMode = false
						s.selected_y = 0
						s.refreshGitStatus(true)
						s.rebuildRenderLines()
					}
				}
				return
			case tcell.KeyUp:
				if s.selected_y > 0 {
					s.selected_y--
					if s.selected_y < s.scroll_y {
						s.scroll_y = s.selected_y
					}
				}
				return
			case tcell.KeyDown:
				if s.selected_y < len(s.branchSearchResults)-1 {
					s.selected_y++
					avail_height := s.view.Height - 3
					if avail_height < 1 {
						avail_height = 1
					}
					if s.selected_y >= s.scroll_y+avail_height {
						s.scroll_y = s.selected_y - avail_height + 1
					}
				}
				return
			case tcell.KeyLeft:
				if s.branchSearchCursor > 0 {
					s.branchSearchCursor--
				}
				return
			case tcell.KeyRight:
				runes := []rune(s.branchSearchInput)
				if s.branchSearchCursor < len(runes) {
					s.branchSearchCursor++
				}
				return
			case tcell.KeyBackspace, tcell.KeyBackspace2:
				runes := []rune(s.branchSearchInput)
				if s.branchSearchCursor > 0 && s.branchSearchCursor <= len(runes) {
					runes = append(runes[:s.branchSearchCursor-1], runes[s.branchSearchCursor:]...)
					s.branchSearchInput = string(runes)
					s.branchSearchCursor--
					s.selected_y = 0
					s.scroll_y = 0
					s.runBranchSearch()
				}
				return
			case tcell.KeyDelete:
				runes := []rune(s.branchSearchInput)
				if s.branchSearchCursor < len(runes) {
					runes = append(runes[:s.branchSearchCursor], runes[s.branchSearchCursor+1:]...)
					s.branchSearchInput = string(runes)
					s.selected_y = 0
					s.scroll_y = 0
					s.runBranchSearch()
				}
				return
			case tcell.KeyRune:
				ch := e.Rune()
				runes := []rune(s.branchSearchInput)
				runes = append(runes[:s.branchSearchCursor], append([]rune{ch}, runes[s.branchSearchCursor:]...)...)
				s.branchSearchInput = string(runes)
				s.branchSearchCursor++
				s.selected_y = 0
				s.scroll_y = 0
				s.runBranchSearch()
				return
			}
		}
		return
	}

	if e, ok := event.(*tcell.EventKey); ok {
		if e.Key() == tcell.KeyCtrlG {
			dir := s.root_dir
			if len(s.parent_tab.Panes) > 1 {
				s.Quit()
			} else {
				s.parent_tab.initSidebar(dir)
				s.Quit()
			}
			return
		}
		if e.Key() == tcell.KeyCtrlW {
			s.Quit()
			return
		}
		if e.Key() == tcell.KeyEscape {
			dir := s.root_dir
			if len(s.parent_tab.Panes) > 1 {
				s.Quit()
				s.parent_tab.initSidebar(dir)
			} else {
				s.parent_tab.initSidebar(dir)
				s.Quit()
			}
			for _, p := range s.parent_tab.Panes {
				if s_pane, ok := p.(*SidebarPane); ok {
					idx := s.parent_tab.GetPane(s_pane.ID())
					s.parent_tab.SetActive(idx)
					break
				}
			}
			return
		}

		// Check if we are interacting with commit message text input
		var selectedItem RenderLine
		if s.selected_y >= 0 && s.selected_y < len(s.renderLines) {
			selectedItem = s.renderLines[s.selected_y]
		}

		if selectedItem.itemType == "commit" {
			// Inside commit input box
			switch e.Key() {
			case tcell.KeyUp:
				s.selectPrev()
				return
			case tcell.KeyDown:
				s.selectNext()
				return
			case tcell.KeyLeft:
				if s.commitCursor > 0 {
					s.commitCursor--
				}
				return
			case tcell.KeyRight:
				runes := []rune(s.commitInput)
				if s.commitCursor < len(runes) {
					s.commitCursor++
				}
				return
			case tcell.KeyBackspace, tcell.KeyBackspace2:
				runes := []rune(s.commitInput)
				if s.commitCursor > 0 && s.commitCursor <= len(runes) {
					newRunes := append(runes[:s.commitCursor-1], runes[s.commitCursor:]...)
					s.commitInput = string(newRunes)
					s.commitCursor--
					s.rebuildRenderLines()
				}
				return
			case tcell.KeyDelete:
				runes := []rune(s.commitInput)
				if s.commitCursor < len(runes) {
					newRunes := append(runes[:s.commitCursor], runes[s.commitCursor+1:]...)
					s.commitInput = string(newRunes)
					s.rebuildRenderLines()
				}
				return
			case tcell.KeyRune:
				ch := e.Rune()
				runes := []rune(s.commitInput)
				if s.commitCursor > len(runes) {
					s.commitCursor = len(runes)
				}
				newRunes := make([]rune, len(runes)+1)
				copy(newRunes, runes[:s.commitCursor])
				newRunes[s.commitCursor] = ch
				copy(newRunes[s.commitCursor+1:], runes[s.commitCursor:])
				s.commitInput = string(newRunes)
				s.commitCursor++
				s.rebuildRenderLines()
				return
			case tcell.KeyEnter:
				if strings.TrimSpace(s.commitInput) != "" {
					cmd := exec.Command("git", "commit", "-m", s.commitInput)
					cmd.Dir = s.root_dir
					_ = cmd.Run()
					s.commitInput = ""
					s.commitCursor = 0
					s.refreshGitStatus(true)
					s.rebuildRenderLines()
				}
				return
			}
		}

		// Standard navigation & actions for non-commit-input rows
		switch e.Key() {
		case tcell.KeyDelete:
			if selectedItem.itemType == "staged" || selectedItem.itemType == "change" {
				InfoBar.YNPrompt("Discard all changes in "+selectedItem.path+"? (y,n)", func(yes, canceled bool) {
					if !canceled && yes {
						targetType, targetPath := s.findNextPrevOrNearestFile(selectedItem.path, selectedItem.itemType)
						s.targetSelType = targetType
						s.targetSelPath = targetPath

						cmd := exec.Command("git", "checkout", "HEAD", "--", selectedItem.path)
						cmd.Dir = s.root_dir
						if err := cmd.Run(); err != nil {
							fullPath := filepath.Join(s.root_dir, selectedItem.path)
							_ = os.RemoveAll(fullPath)
						}
						s.refreshGitStatus(true)
						s.rebuildRenderLines()
					}
				})
			}
			return
		case tcell.KeyUp:
			s.selectPrev()
			return
		case tcell.KeyDown:
			s.selectNext()
			return
		case tcell.KeyRight:
			if selectedItem.itemType == "staged" || selectedItem.itemType == "change" {
				fullPath := filepath.Join(s.root_dir, selectedItem.path)
				if _, err := os.Stat(fullPath); err == nil {
					s.openFileInWorkspace(selectedItem.path)
				}
			}
			return
		case tcell.KeyEnter:
			if e.Modifiers()&tcell.ModShift != 0 {
				// Shift+Enter to open file if not deleted
				if selectedItem.itemType == "staged" || selectedItem.itemType == "change" {
					fullPath := filepath.Join(s.root_dir, selectedItem.path)
					if _, err := os.Stat(fullPath); err == nil {
						s.openFileInWorkspace(selectedItem.path)
					}
				}
				return
			}

			// Enter actions
			switch selectedItem.itemType {
			case "branch":
				s.branchSearchMode = true
				s.branchSearchInput = ""
				s.branchSearchCursor = 0
				s.selected_y = 0
				s.scroll_y = 0
				s.loadBranches()
				s.runBranchSearch()
				return
			case "pull":
				cmd := exec.Command("git", "pull")
				cmd.Dir = s.root_dir
				_ = cmd.Run()
				s.refreshGitStatus(true)
				s.rebuildRenderLines()
			case "push":
				cmd := exec.Command("git", "push")
				cmd.Dir = s.root_dir
				_ = cmd.Run()
				s.refreshGitStatus(true)
				s.rebuildRenderLines()
			case "staged":
				targetType, targetPath := s.findNextPrevOrNearestFile(selectedItem.path, "staged")
				s.targetSelType = targetType
				s.targetSelPath = targetPath

				cmd := exec.Command("git", "reset", "HEAD", selectedItem.path)
				cmd.Dir = s.root_dir
				_ = cmd.Run()
				s.refreshGitStatus(true)
				s.rebuildRenderLines()
			case "change":
				targetType, targetPath := s.findNextPrevOrNearestFile(selectedItem.path, "change")
				s.targetSelType = targetType
				s.targetSelPath = targetPath

				cmd := exec.Command("git", "add", selectedItem.path)
				cmd.Dir = s.root_dir
				_ = cmd.Run()
				s.refreshGitStatus(true)
				s.rebuildRenderLines()
			}
			return
		}
	} else if e, ok := event.(*tcell.EventMouse); ok {
		_, my := e.Position()
		btn := e.Buttons()
		if btn == tcell.WheelDown {
			maxScroll := len(s.renderLines) - 1
			if s.isGitRepo() {
				maxScroll = len(s.renderLines) - 2
			}
			if s.scroll_y < maxScroll {
				s.scroll_y++
			}
		} else if btn == tcell.WheelUp {
			if s.scroll_y > 0 {
				s.scroll_y--
			}
		} else if btn == tcell.Button1 {
			clickY := my - s.view.Y
			if s.isGitRepo() && clickY == 1 {
				// Clicked on branch name!
				if s.selected_y == 0 {
					s.branchSearchMode = true
					s.branchSearchInput = ""
					s.branchSearchCursor = 0
					s.selected_y = 0
					s.scroll_y = 0
					s.loadBranches()
					s.runBranchSearch()
				} else {
					s.selected_y = 0
				}
			} else {
				row_clicked := clickY - 3 + s.scroll_y
				if row_clicked >= 0 {
					actualIndex := row_clicked
					if s.isGitRepo() {
						actualIndex = row_clicked + 1
					}
					if actualIndex >= 0 && actualIndex < len(s.renderLines) {
						if s.renderLines[actualIndex].isSel {
							s.selected_y = actualIndex
							if s.renderLines[actualIndex].itemType == "commit" {
								s.commitCursor = len([]rune(s.commitInput))
							}
						}
					}
				}
			}
		}
	}
}

func (s *GitSidebarPane) Quit() {
	s.closed = true
	if len(s.parent_tab.Panes) > 1 {
		n := s.parent_tab.GetNode(s.ID())
		n.Unsplit()
		s.parent_tab.RemovePane(s.parent_tab.GetPane(s.ID()))
		s.parent_tab.Resize()
		s.parent_tab.SetActive(0)
	} else if len(Tabs.List) > 1 {
		Tabs.RemoveTab(s.parent_tab.Panes[0].ID())
	} else {
		screen.Screen.Fini()
		InfoBar.Close()
		os.Exit(0)
	}
}

func (t *Tab) initGitSidebar(dir string) {
	var orig_id uint64
	for _, p := range t.Panes {
		if _, ok := p.(*BufPane); ok {
			orig_id = p.ID()
			break
		}
	}
	if orig_id == 0 && len(t.Panes) > 0 {
		orig_id = t.Panes[0].ID()
	}
	if orig_id == 0 {
		orig_id = t.Node.ID()
	}

	s := NewGitSidebarPane(dir, t)
	right_id := t.GetNode(orig_id).VSplit(true)
	s.SetID(orig_id)
	var e Pane
	for _, p := range t.Panes {
		if p.ID() == orig_id {
			e = p
			break
		}
	}
	if e != nil {
		e.SetID(right_id)
	}
	t.Panes = append([]Pane{s}, t.Panes...)
	left_node := t.GetNode(s.ID())
	if left_node != nil {
		left_node.ResizeSplit(64)
	}
	t.Resize()
	t.SetActive(1)
}

func (s *GitSidebarPane) loadBranches() {
	cmd := exec.Command("git", "branch", "-a", "--format=%(refname:short)")
	cmd.Dir = s.root_dir
	out, err := cmd.Output()
	if err != nil {
		s.allBranches = []string{}
		return
	}
	lines := strings.Split(string(out), "\n")
	var branches []string
	seen := make(map[string]bool)
	for _, line := range lines {
		b := strings.TrimSpace(line)
		if b == "" || strings.Contains(b, "HEAD ->") || strings.Contains(b, "/HEAD") {
			continue
		}
		if !seen[b] {
			seen[b] = true
			branches = append(branches, b)
		}
	}
	s.allBranches = branches
}

func (s *GitSidebarPane) runBranchSearch() {
	s.branchSearchResults = []string{}
	query := strings.ToLower(s.branchSearchInput)
	for _, b := range s.allBranches {
		if query == "" || strings.Contains(strings.ToLower(b), query) {
			s.branchSearchResults = append(s.branchSearchResults, b)
		}
	}
}

