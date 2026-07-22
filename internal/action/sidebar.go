package action

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"

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

type SidebarPane struct {
	view       *display.View
	is_active  bool
	pane_id    uint64
	parent_tab *Tab

	root_dir    string
	root_node   *FileNode
	scroll_y    int
	selected_y  int
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
	return s
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

	// Line 0: Title
	grey_color, _ := config.StringToColor("brightblack")
	grey_style := config.DefStyle.Foreground(grey_color)
	title_str := filepath.Base(s.root_dir)
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
