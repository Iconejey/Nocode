package action

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micro-editor/micro/v2/internal/buffer"
	"github.com/micro-editor/micro/v2/internal/config"
	"github.com/micro-editor/micro/v2/internal/display"
	"github.com/micro-editor/micro/v2/internal/screen"
	"github.com/micro-editor/micro/v2/internal/views"
	"github.com/micro-editor/tcell/v2"
)

type FileNode struct {
	name     string
	path     string
	is_dir   bool
	is_open  bool
	children []*FileNode
	parent   *FileNode
}

type FlatNode struct {
	node  *FileNode
	level int
}

type SearchResult struct {
	path        string
	line_number int
	line        string
}

type SidebarPane struct {
	view       *display.View
	is_active  bool
	pane_id    uint64
	parent_tab *Tab

	root_dir    string
	root_node   *FileNode
	scroll_y    int
	selected_y  int

	git_status_cache map[string]string
	last_git_refresh time.Time

	search_mode    bool
	search_type    int // 0: file, 1: text
	search_input   string
	search_cursor  int
	search_results []SearchResult

	closed bool
	mutex  sync.Mutex
}

func NewSidebarPane(dir string, tab *Tab) *SidebarPane {
	s := &SidebarPane{
		view:       &display.View{},
		parent_tab: tab,
		root_dir:   dir,
	}
	s.root_node = &FileNode{
		name:    filepath.Base(dir),
		path:    dir,
		is_dir:  true,
		is_open: true,
	}
	s.loadNodeChildren(s.root_node)

	go s.watchWorkspace()

	return s
}

func (s *SidebarPane) updateGitStatus() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.updateGitStatusLocked()
}

func (s *SidebarPane) updateGitStatusLocked() {
	if s.root_dir == "" { return }

	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = s.root_dir
	out_bytes, err := cmd.Output()
	if err != nil {
		s.git_status_cache = make(map[string]string)
		return
	}

	repo_root := strings.TrimSpace(string(out_bytes))
	repo_root, err = filepath.EvalSymlinks(repo_root)
	if err != nil { repo_root = strings.TrimSpace(string(out_bytes)) }

	cmd = exec.Command("git", "status", "--ignored", "--porcelain", "-z")
	cmd.Dir = s.root_dir
	status_bytes, err := cmd.Output()
	if err != nil {
		s.git_status_cache = make(map[string]string)
		return
	}

	new_cache := make(map[string]string)
	parts := strings.Split(string(status_bytes), "\x00")

	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if len(part) < 4 { continue }
		status := part[0:2]
		path := part[3:]

		if status[0] == 'R' || status[0] == 'C' {
			i++
			if i < len(parts) { path = parts[i] }
		}

		abs_path := filepath.Join(repo_root, path)
		abs_path, err = filepath.EvalSymlinks(abs_path)
		if err != nil { abs_path = filepath.Join(repo_root, path) }

		color := ""
		if status[0] == '?' || status[1] == '?' || status[0] == 'A' || status[1] == 'A' {
			color = "green"
		} else if status[0] == 'M' || status[1] == 'M' || status[0] == 'R' || status[1] == 'R' || status[0] == 'C' || status[1] == 'C' {
			color = "blue"
		} else if status[0] == '!' && status[1] == '!' {
			color = "brightblack"
		}

		if color != "" { new_cache[abs_path] = color }
	}

	folder_colors := make(map[string]string)
	for file_path, color := range new_cache {
		if color == "brightblack" {
			continue
		}
		dir := filepath.Dir(file_path)
		for {
			if !strings.HasPrefix(dir, repo_root) || dir == filepath.Dir(dir) { break }
			existing := folder_colors[dir]
			if existing != "blue" { folder_colors[dir] = color }
			dir = filepath.Dir(dir)
		}
	}

	for dir_path, color := range folder_colors {
		new_cache[dir_path] = color
	}

	s.git_status_cache = new_cache
	s.last_git_refresh = time.Now()
}

func (s *SidebarPane) getGitColor(node_path string) string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.git_status_cache == nil {
		s.updateGitStatusLocked()
	} else if time.Since(s.last_git_refresh) > 2*time.Second {
		s.updateGitStatusLocked()
	}

	clean_path := filepath.Clean(node_path)
	abs_path, err := filepath.Abs(clean_path)
	if err == nil {
		clean_path = abs_path
	}
	eval_path, err := filepath.EvalSymlinks(clean_path)
	if err == nil {
		clean_path = eval_path
	}
	if color, ok := s.git_status_cache[clean_path]; ok { return color }

	abs_root_dir, err := filepath.Abs(s.root_dir)
	if err == nil {
		eval_root, err := filepath.EvalSymlinks(abs_root_dir)
		if err == nil {
			abs_root_dir = eval_root
		}
	} else {
		abs_root_dir = s.root_dir
	}

	dir := filepath.Dir(clean_path)
	for {
		if color, ok := s.git_status_cache[dir]; ok && color == "brightblack" {
			return "brightblack"
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(clean_path, abs_root_dir) {
			break
		}
		dir = parent
	}

	return ""
}

func (s *SidebarPane) loadNodeChildren(node *FileNode) {
	if node.is_dir && len(node.children) == 0 {
		children, err := readDir(node.path)
		if err == nil {
			node.children = children
			for _, child := range children {
				child.parent = node
			}
		}
	}
}

func readDir(path string) ([]*FileNode, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var children []*FileNode
	for _, entry := range entries {
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		child := &FileNode{
			name:    entry.Name(),
			path:    filepath.Join(path, entry.Name()),
			is_dir:  entry.IsDir(),
			is_open: false,
		}
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].is_dir != children[j].is_dir {
			return children[i].is_dir
		}
		return children[i].name < children[j].name
	})
	return children, nil
}

func (s *SidebarPane) ID() uint64 {
	return s.pane_id
}

func (s *SidebarPane) SetID(i uint64) {
	s.pane_id = i
}

func (s *SidebarPane) Name() string {
	return "Workspace"
}

func (s *SidebarPane) Close() {
}

func (s *SidebarPane) SetTab(t *Tab) {
	s.parent_tab = t
}

func (s *SidebarPane) Tab() *Tab {
	return s.parent_tab
}

func (s *SidebarPane) getFlatNodes() []*FlatNode {
	flat_nodes := []*FlatNode{}
	for _, child := range s.root_node.children {
		flat_nodes = append(flat_nodes, s.getFlatNodesForNode(child, 0)...)
	}
	return flat_nodes
}

func (s *SidebarPane) getFlatNodesForNode(node *FileNode, level int) []*FlatNode {
	flat_nodes := []*FlatNode{}
	flat_nodes = append(flat_nodes, &FlatNode{node: node, level: level})
	if node.is_open && node.is_dir {
		for _, child := range node.children {
			flat_nodes = append(flat_nodes, s.getFlatNodesForNode(child, level+1)...)
		}
	}
	return flat_nodes
}

func (s *SidebarPane) Display() {
	s.Clear()

	if s.search_mode {
		var title string
		if s.search_type == 0 {
			title = "FILE SEARCH"
		} else {
			title = "TEXT SEARCH"
		}
		grey_color, _ := config.StringToColor("brightblack")
		grey_style := config.DefStyle.Foreground(grey_color)

		for x := 0; x < s.view.Width; x++ {
			var r rune = ' '
			if x < len(title) {
				r = rune(title[x])
			}
			screen.SetContent(s.view.X+x, s.view.Y, r, nil, grey_style)
		}

		prompt := "> "
		input_str := prompt + s.search_input
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

		if s.is_active {
			screen.ShowCursor(s.view.X+len(prompt)+s.search_cursor, s.view.Y+1)
		}

		for x := 0; x < s.view.Width; x++ {
			screen.SetContent(s.view.X+x, s.view.Y+2, ' ', nil, config.DefStyle)
		}

		blue_color, _ := config.StringToColor("blue")
		blue_style := config.DefStyle.Foreground(blue_color)

		avail_height := s.view.Height - 3
		if avail_height < 1 {
			avail_height = 1
		}

		for y := 3; y < s.view.Height; y++ {
			index := (y - 3) + s.scroll_y
			if index >= len(s.search_results) {
				break
			}
			result := s.search_results[index]

			var line_style tcell.Style
			if index == s.selected_y {
				line_style = white_style.Reverse(true)
			} else {
				line_style = white_style
			}

			if s.search_type == 0 {
				dir_part := filepath.Dir(result.path)
				base_part := filepath.Base(result.path)
				
				var display_str string
				var dir_len int
				if dir_part == "." || dir_part == "" {
					display_str = base_part
					dir_len = 0
				} else {
					display_str = dir_part + string(filepath.Separator) + base_part
					dir_len = len([]rune(dir_part + string(filepath.Separator)))
				}

				runes := []rune(display_str)
				offset := len(runes) - s.view.Width
				if offset < 0 {
					offset = 0
				}
				for x := 0; x < s.view.Width; x++ {
					var r rune = ' '
					var cur_style tcell.Style
					orig_idx := x + offset
					if index == s.selected_y {
						cur_style = white_style.Reverse(true)
					} else {
						if orig_idx < dir_len {
							cur_style = grey_style
						} else {
							cur_style = white_style
						}
					}
					if orig_idx < len(runes) {
						r = runes[orig_idx]
					}
					screen.SetContent(s.view.X+x, s.view.Y+y, r, nil, cur_style)
				}
			} else {
				prefix := result.path + ":" + strconv.Itoa(result.line_number) + " "
				full_str := prefix + result.line
				runes := []rune(full_str)

				query := strings.ToLower(s.search_input)
				match_indices := []int{}
				if query != "" {
					line_lower := strings.ToLower(result.line)
					start := 0
					for {
						idx := strings.Index(line_lower[start:], query)
						if idx == -1 {
							break
						}
						match_indices = append(match_indices, len(prefix)+start+idx)
						start += idx + len(query)
					}
				}

				offset := len(runes) - s.view.Width
				if offset < 0 {
					offset = 0
				}
				for x := 0; x < s.view.Width; x++ {
					var r rune = ' '
					var cur_style = line_style

					orig_idx := x + offset
					if orig_idx < len(runes) {
						r = runes[orig_idx]
						if index == s.selected_y {
							cur_style = line_style
						} else {
							if orig_idx < len(prefix) {
								cur_style = grey_style
							} else {
								is_match := false
								for _, start_idx := range match_indices {
									if orig_idx >= start_idx && orig_idx < start_idx+len(query) {
										is_match = true
										break
									}
								}
								if is_match {
									cur_style = blue_style
								} else {
									cur_style = line_style
								}
							}
						}
					}
					screen.SetContent(s.view.X+x, s.view.Y+y, r, nil, cur_style)
				}
			}
		}
		return
	}

	// Line 0: Title
	grey_color, _ := config.StringToColor("brightblack")
	grey_style := config.DefStyle.Foreground(grey_color)
	abs_dir, err := filepath.Abs(s.root_dir)
	title_str := filepath.Base(s.root_dir)
	if err == nil { title_str = filepath.Base(abs_dir) }
	title_runes := []rune(title_str)
	for x := 0; x < s.view.Width; x++ {
		var r rune = ' '
		if x < len(title_runes) {
			r = title_runes[x]
		}
		screen.SetContent(s.view.X+x, s.view.Y, r, nil, grey_style)
	}

	// Line 1: Empty line
	for x := 0; x < s.view.Width; x++ {
		screen.SetContent(s.view.X+x, s.view.Y+1, ' ', nil, config.DefStyle)
	}

	// Line 2+: File tree
	flat_nodes := s.getFlatNodes()
	white_color, _ := config.StringToColor("brightwhite")
	white_style := config.DefStyle.Foreground(white_color)

	for y := 2; y < s.view.Height; y++ {
		index := (y - 2) + s.scroll_y
		if index >= len(flat_nodes) {
			break
		}
		flat_node := flat_nodes[index]
		style := white_style

		git_color := s.getGitColor(flat_node.node.path)

		if git_color != "" {
			if color, ok := config.StringToColor(git_color); ok {
				style = config.DefStyle.Foreground(color)
			}
		}

		if index == s.selected_y {
			style = style.Reverse(true)
		}

		indent := ""
		for i := 0; i < flat_node.level; i++ {
			indent += "  "
		}

		prefix := ""
		prefix_style := style
		if flat_node.node.is_dir {
			if flat_node.node.is_open {
				prefix = "- "
			} else {
				prefix = "+ "
			}
			if index != s.selected_y {
				prefix_style = grey_style
			} else {
				prefix_style = style
			}
		} else {
			prefix = "  "
		}

		line_str := indent + prefix + flat_node.node.name
		runes := []rune(line_str)
		for x := 0; x < s.view.Width; x++ {
			var r rune = ' '
			var cur_style tcell.Style = style
			if x < len(runes) {
				r = runes[x]
				if flat_node.node.is_dir && x >= len(indent) && x < len(indent)+2 {
					cur_style = prefix_style
				}
			}
			screen.SetContent(s.view.X+x, s.view.Y+y, r, nil, cur_style)
		}
	}
}

func (s *SidebarPane) Clear() {
	for y := 0; y < s.view.Height; y++ {
		for x := 0; x < s.view.Width; x++ {
			screen.SetContent(s.view.X+x, s.view.Y+y, ' ', nil, config.DefStyle)
		}
	}
}

func (s *SidebarPane) Relocate() bool {
	return false
}

func (s *SidebarPane) GetView() *display.View {
	return s.view
}

func (s *SidebarPane) SetView(v *display.View) {
	s.view = v
}

func (s *SidebarPane) LocFromVisual(vloc buffer.Loc) buffer.Loc {
	return vloc
}

func (s *SidebarPane) Resize(w, h int) {
	s.view.Width = w
	s.view.Height = h
}

func (s *SidebarPane) SetActive(b bool) {
	s.is_active = b
}

func (s *SidebarPane) IsActive() bool {
	return s.is_active
}

func (s *SidebarPane) HandleCommand(cmd string) {
}

func (s *SidebarPane) toggleOrOpenNode(node *FileNode) {
	if node.is_dir {
		node.is_open = !node.is_open
		if node.is_open {
			s.loadNodeChildren(node)
		}
	} else {
		s.openFileInWorkspace(node.path)
	}
}

func (s *SidebarPane) openFileInWorkspace(path string) {
	b, err := buffer.NewBufferFromFile(path, buffer.BTDefault)
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
				s.parent_tab.Resize()
				left_node := s.parent_tab.GetNode(s.ID())
				if left_node != nil {
					left_node.ResizeSplit(32)
				}
				s.parent_tab.SetActive(1)
			}
		}
	}
}

func (s *SidebarPane) HandleEvent(event tcell.Event) {
	if s.search_mode {
		if e, ok := event.(*tcell.EventKey); ok {
			switch e.Key() {
			case tcell.KeyEscape:
				s.search_mode = false
				s.parent_tab.Resize()
				return
			case tcell.KeyCtrlW:
				s.Quit()
				return
			case tcell.KeyEnter:
				if s.selected_y >= 0 && s.selected_y < len(s.search_results) {
					s.selectSearchResult(s.search_results[s.selected_y])
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
				if s.selected_y < len(s.search_results)-1 {
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
				if s.search_cursor > 0 {
					s.search_cursor--
				}
				return
			case tcell.KeyRight:
				runes := []rune(s.search_input)
				if s.search_cursor < len(runes) {
					s.search_cursor++
				}
				return
			case tcell.KeyBackspace, tcell.KeyBackspace2:
				runes := []rune(s.search_input)
				if s.search_cursor > 0 && s.search_cursor <= len(runes) {
					runes = append(runes[:s.search_cursor-1], runes[s.search_cursor:]...)
					s.search_input = string(runes)
					s.search_cursor--
					s.selected_y = 0
					s.scroll_y = 0
					if s.search_type == 0 {
						s.runFileSearch()
					} else {
						s.runTextSearch()
					}
				}
				return
			case tcell.KeyDelete:
				runes := []rune(s.search_input)
				if s.search_cursor < len(runes) {
					runes = append(runes[:s.search_cursor], runes[s.search_cursor+1:]...)
					s.search_input = string(runes)
					s.selected_y = 0
					s.scroll_y = 0
					if s.search_type == 0 {
						s.runFileSearch()
					} else {
						s.runTextSearch()
					}
				}
				return
			case tcell.KeyRune:
				ch := e.Rune()
				runes := []rune(s.search_input)
				runes = append(runes[:s.search_cursor], append([]rune{ch}, runes[s.search_cursor:]...)...)
				s.search_input = string(runes)
				s.search_cursor++
				s.selected_y = 0
				s.scroll_y = 0
				if s.search_type == 0 {
					s.runFileSearch()
				} else {
					s.runTextSearch()
				}
				return
			}
		} else if e, ok := event.(*tcell.EventMouse); ok {
			_, my := e.Position()
			btn := e.Buttons()
			avail_height := s.view.Height - 3
			if avail_height < 1 {
				avail_height = 1
			}
			if btn == tcell.WheelDown {
				if s.scroll_y < len(s.search_results)-avail_height {
					s.scroll_y++
				}
			} else if btn == tcell.WheelUp {
				if s.scroll_y > 0 {
					s.scroll_y--
				}
			} else if btn == tcell.Button1 {
				y_clicked := my - s.view.Y
				if y_clicked >= 3 {
					row_clicked := (y_clicked - 3) + s.scroll_y
					if row_clicked >= 0 && row_clicked < len(s.search_results) {
						s.selected_y = row_clicked
						s.selectSearchResult(s.search_results[row_clicked])
					}
				}
			}
		}
		return
	}

	avail_height := s.view.Height - 2
	if avail_height < 1 {
		avail_height = 1
	}

	if e, ok := event.(*tcell.EventKey); ok {
		if e.Key() == tcell.KeyCtrlW {
			s.Quit()
			return
		}
		if e.Key() == tcell.KeyRune && (e.Rune() == 'b' || e.Rune() == 'B') && e.Modifiers()&tcell.ModAlt != 0 {
			s.Quit()
			return
		}
		flat_nodes := s.getFlatNodes()
		if e.Key() == tcell.KeyDown {
			if s.selected_y < len(flat_nodes)-1 {
				s.selected_y++
				if s.selected_y >= s.scroll_y+avail_height {
					s.scroll_y = s.selected_y - avail_height + 1
				}
			}
		} else if e.Key() == tcell.KeyUp {
			if s.selected_y > 0 {
				s.selected_y--
				if s.selected_y < s.scroll_y {
					s.scroll_y = s.selected_y
				}
			}
		} else if e.Key() == tcell.KeyEnter || e.Rune() == ' ' {
			if s.selected_y >= 0 && s.selected_y < len(flat_nodes) {
				s.toggleOrOpenNode(flat_nodes[s.selected_y].node)
			}
		}
	} else if e, ok := event.(*tcell.EventMouse); ok {
		_, my := e.Position()
		btn := e.Buttons()
		if btn == tcell.WheelDown {
			flat_nodes := s.getFlatNodes()
			if s.scroll_y < len(flat_nodes)-avail_height {
				s.scroll_y++
			}
		} else if btn == tcell.WheelUp {
			if s.scroll_y > 0 {
				s.scroll_y--
			}
		} else if btn == tcell.Button1 {
			y_clicked := my - s.view.Y
			if y_clicked >= 2 {
				row_clicked := (y_clicked - 2) + s.scroll_y
				flat_nodes := s.getFlatNodes()
				if row_clicked >= 0 && row_clicked < len(flat_nodes) {
					s.selected_y = row_clicked
					s.toggleOrOpenNode(flat_nodes[row_clicked].node)
				}
			}
		}
	}
}

func (s *SidebarPane) Quit() {
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
		runtime.Goexit()
	}
}

func (t *Tab) initSidebar(dir string) {
	orig_id := t.Node.ID()
	s := NewSidebarPane(dir, t)
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
		left_node.ResizeSplit(32)
	}
	t.Resize()
	t.SetActive(1)
}

func NewTabWithSidebarOnly(x, y, width, height int, dir string) *Tab {
	t := new(Tab)
	t.Node = views.NewRoot(x, y, width, height)
	t.UIWindow = display.NewUIWindow(t.Node)
	t.release = true

	s := NewSidebarPane(dir, t)
	s.SetID(t.ID())

	t.Panes = []Pane{s}
	t.SetActive(0)
	return t
}

func (s *SidebarPane) runFileSearch() {
	s.search_results = nil
	if s.root_dir == "" {
		return
	}
	query := strings.ToLower(s.search_input)

	_ = filepath.WalkDir(s.root_dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".idea" || name == ".vscode" || name == "vendor" {
				return filepath.SkipDir
			}
			if s.getGitColor(path) == "brightblack" {
				return filepath.SkipDir
			}
			return nil
		}

		if s.getGitColor(path) == "brightblack" {
			return nil
		}

		rel, err := filepath.Rel(s.root_dir, path)
		if err != nil {
			rel = path
		}

		if query == "" || strings.Contains(strings.ToLower(rel), query) {
			s.search_results = append(s.search_results, SearchResult{
				path:        rel,
				line_number: 0,
				line:        "",
			})
			if len(s.search_results) >= 1000 {
				return errors.New("limit reached")
			}
		}
		return nil
	})
}

func (s *SidebarPane) runTextSearch() {
	s.search_results = nil
	if s.root_dir == "" || s.search_input == "" {
		return
	}
	query := strings.ToLower(s.search_input)

	_ = filepath.WalkDir(s.root_dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".idea" || name == ".vscode" || name == "vendor" {
				return filepath.SkipDir
			}
			if s.getGitColor(path) == "brightblack" {
				return filepath.SkipDir
			}
			return nil
		}

		if s.getGitColor(path) == "brightblack" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > 10*1024*1024 { // skip files larger than 10MB
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Heuristic: check if binary
		isBinary := false
		limit := 1024
		if len(content) < limit {
			limit = len(content)
		}
		for i := 0; i < limit; i++ {
			if content[i] == 0 {
				isBinary = true
				break
			}
		}
		if isBinary {
			return nil
		}

		rel, err := filepath.Rel(s.root_dir, path)
		if err != nil {
			rel = path
		}

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			if strings.Contains(strings.ToLower(line), query) {
				s.search_results = append(s.search_results, SearchResult{
					path:        rel,
					line_number: i + 1,
					line:        line,
				})
				if len(s.search_results) >= 1000 {
					return errors.New("limit reached")
				}
			}
		}
		return nil
	})
}

func (s *SidebarPane) selectSearchResult(result SearchResult) {
	full_path := filepath.Join(s.root_dir, result.path)
	b, err := buffer.NewBufferFromFile(full_path, buffer.BTDefault)
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

		var bp *BufPane
		if active_editor_idx != -1 {
			bp = s.parent_tab.Panes[active_editor_idx].(*BufPane)
			bp.VSplitBuf(b)
		} else {
			for _, p := range s.parent_tab.Panes {
				if b_pane, ok := p.(*BufPane); ok {
					bp = b_pane
					break
				}
			}
			if bp != nil {
				bp.VSplitBuf(b)
			} else {
				bp = NewBufPaneFromBuf(b, s.parent_tab)
				bp.splitID = s.parent_tab.GetNode(s.pane_id).VSplit(true)
				s.parent_tab.AddPane(bp, 1)
				s.parent_tab.Resize()
				left_node := s.parent_tab.GetNode(s.ID())
				if left_node != nil {
					left_node.ResizeSplit(32)
				}
				s.parent_tab.SetActive(1)
			}
		}

		if result.line_number > 0 && bp != nil {
			bp.GotoLoc(buffer.Loc{X: 0, Y: result.line_number - 1})
		}

		if bp != nil {
			idx := s.parent_tab.GetPane(bp.ID())
			s.parent_tab.SetActive(idx)
		}
	}

	s.search_mode = false
	s.parent_tab.Resize()
}

func (s *SidebarPane) watchWorkspace() {
	last_state := s.getWorkspaceState()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		if s.closed {
			return
		}
		select {
		case <-ticker.C:
			if s.closed {
				return
			}
			current_state := s.getWorkspaceState()
			if !statesEqual(last_state, current_state) {
				last_state = current_state
				s.updateGitStatus()
				s.mutex.Lock()
				s.RefreshTree(s.root_node)
				s.mutex.Unlock()
				screen.Redraw()
			}
		}
	}
}

func (s *SidebarPane) getWorkspaceState() map[string]int64 {
	state := make(map[string]int64)
	if s.root_dir == "" {
		return state
	}
	_ = filepath.WalkDir(s.root_dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == ".idea" || name == ".vscode" || name == "vendor" {
				return filepath.SkipDir
			}
		}
		info, err := d.Info()
		if err == nil {
			state[path] = info.ModTime().UnixNano()
		}
		return nil
	})
	return state
}

func statesEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (s *SidebarPane) RefreshTree(node *FileNode) {
	if !node.is_dir {
		return
	}
	if len(node.children) > 0 {
		new_children, err := readDir(node.path)
		if err == nil {
			open_dirs := make(map[string]bool)
			for _, child := range node.children {
				if child.is_dir && child.is_open {
					open_dirs[child.name] = true
				}
			}

			existing_nodes := make(map[string]*FileNode)
			for _, child := range node.children {
				existing_nodes[child.name] = child
			}

			node.children = new_children
			for _, child := range node.children {
				child.parent = node
				if child.is_dir {
					if open_dirs[child.name] {
						child.is_open = true
						if old_child, ok := existing_nodes[child.name]; ok {
							child.children = old_child.children
						}
					}
					s.RefreshTree(child)
				}
			}
		}
	}
}
