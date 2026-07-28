package action

import (
	"strings"

	"github.com/micro-editor/micro/v2/internal/buffer"
	"github.com/micro-editor/micro/v2/internal/display"
	"github.com/micro-editor/micro/v2/internal/views"
	dmp "github.com/sergi/go-diff/diffmatchpatch"
)

// SplitDiffCmd toggles the split-diff view for the current buffer
func (h *BufPane) SplitDiffCmd(args []string) {
	h.SplitDiffAction()
}

// SplitDiffAction toggles the split-diff view for the current buffer
func (h *BufPane) SplitDiffAction() bool {
	t := h.tab
	if t.InSplitDiff {
		// Exit split diff and restore original panes
		t.InSplitDiff = false
		t.Node = t.SavedNode
		t.Panes = t.SavedPanes
		t.UIWindow = display.NewUIWindow(t.Node)
		t.active = t.SavedActive

		// Reset InSplitDiff on the restored buffers so they render normally again
		for _, p := range t.Panes {
			if bp, ok := p.(*BufPane); ok && bp.Buf != nil {
				bp.Buf.InSplitDiff = false
				bp.Buf.DiffLines = nil
			}
		}

		t.Resize()
		t.SetActive(t.active)
		return true
	}

	// Enter split diff mode
	if h.Buf == nil {
		return false
	}

	// 1. Get the old version (diff base) and new version of the buffer content
	oldText := string(h.Buf.DiffBase())
	newText := string(h.Buf.Bytes())

	// 2. Compute the aligned left and right DiffLines
	leftLines, rightLines := AlignDiff(oldText, newText)

	// 3. Save current tab state (with scroll positions)
	t.SavedNode = t.Node
	t.SavedPanes = t.Panes
	t.SavedActive = t.active
	t.InSplitDiff = true

	// 4. Create Left buffer (Old version)
	oldBuf := buffer.NewBufferFromString(oldText, h.Buf.GetName()+" (Old)", buffer.BTDefault)
	oldBuf.Settings["readonly"] = true
	oldBuf.InSplitDiff = true
	oldBuf.IsLeftDiff = true
	oldBuf.DiffLines = leftLines

	if ft, ok := h.Buf.Settings["filetype"]; ok {
		oldBuf.Settings["filetype"] = ft
		oldBuf.UpdateRules()
	}

	// Set original/new buffer properties
	h.Buf.InSplitDiff = true
	h.Buf.IsLeftDiff = false
	h.Buf.DiffLines = rightLines

	// 5. Create new node tree with a vertical split
	t.Node = views.NewRoot(t.X, t.Y, t.W, t.H)
	t.UIWindow = display.NewUIWindow(t.Node)

	// Create Left and Right panes
	leftPane := NewBufPaneFromBuf(oldBuf, t)
	rightPane := NewBufPaneFromBuf(h.Buf, t)

	leftPane.SetID(t.ID())
	t.Panes = []Pane{leftPane}
	t.active = 0

	rightPane.splitID = t.GetNode(leftPane.splitID).VSplit(true)
	t.AddPane(rightPane, 1)

	// Set Right pane as the active one
	t.active = 1

	// Synchronize scroll positions initially!
	// Scroll both panes to the active pane's original scroll position
	activeView := h.GetView()
	leftPane.GetView().StartLine = activeView.StartLine
	leftPane.GetView().StartCol = activeView.StartCol
	rightPane.GetView().StartLine = activeView.StartLine
	rightPane.GetView().StartCol = activeView.StartCol

	t.Resize()
	t.SetActive(t.active)

	return true
}

func AlignDiff(oldText, newText string) ([]buffer.AlignedLine, []buffer.AlignedLine) {
	oldText = strings.ReplaceAll(oldText, "\r\n", "\n")
	newText = strings.ReplaceAll(newText, "\r\n", "\n")

	d := dmp.New()
	aRunes, bRunes, lineArray := d.DiffLinesToRunes(oldText, newText)
	diffs := d.DiffMainRunes(aRunes, bRunes, false)
	diffs = d.DiffCleanupSemantic(diffs)

	type lineInfo struct {
		content string
	}

	type diffBlock struct {
		op    dmp.Operation
		lines []lineInfo
	}

	var blocks []diffBlock

	for _, diff := range diffs {
		diffRunes := []rune(diff.Text)
		var blockLines []lineInfo
		for _, r := range diffRunes {
			lineIdx := int(r)
			lineStr := ""
			if lineIdx >= 0 && lineIdx < len(lineArray) {
				lineStr = lineArray[lineIdx]
			}
			lineStr = strings.TrimSuffix(lineStr, "\n")
			lineStr = strings.TrimSuffix(lineStr, "\r")

			blockLines = append(blockLines, lineInfo{content: lineStr})
		}
		if len(blockLines) > 0 {
			blocks = append(blocks, diffBlock{op: diff.Type, lines: blockLines})
		}
	}

	var leftResult []buffer.AlignedLine
	var rightResult []buffer.AlignedLine

	leftLineNum := 1
	rightLineNum := 1

	for bIdx := 0; bIdx < len(blocks); bIdx++ {
		block := blocks[bIdx]

		if block.op == dmp.DiffEqual {
			for _, line := range block.lines {
				leftResult = append(leftResult, buffer.AlignedLine{
					Type:    buffer.DiffEqual,
					Content: line.content,
					LineNum: leftLineNum,
				})
				rightResult = append(rightResult, buffer.AlignedLine{
					Type:    buffer.DiffEqual,
					Content: line.content,
					LineNum: rightLineNum,
				})
				leftLineNum++
				rightLineNum++
			}
		} else if block.op == dmp.DiffDelete {
			// Check if the next block is DiffInsert, so we can pair them!
			var nextBlock *diffBlock
			if bIdx+1 < len(blocks) && blocks[bIdx+1].op == dmp.DiffInsert {
				nextBlock = &blocks[bIdx+1]
				bIdx++ // Skip the next block since we process it here!
			}

			if nextBlock != nil {
				D := block.lines
				I := nextBlock.lines
				m := len(D)
				n := len(I)

				dp := make([][]float64, m+1)
				for i := range dp {
					dp[i] = make([]float64, n+1)
				}

				for i := 1; i <= m; i++ {
					for j := 1; j <= n; j++ {
						// Option 1: Skip D[i-1] (delete)
						s1 := dp[i-1][j]
						// Option 2: Skip I[j-1] (insert)
						s2 := dp[i][j-1]
						// Option 3: Match D[i-1] with I[j-1]
						sim := lineSimilarity(D[i-1].content, I[j-1].content)
						var s3 float64
						if sim >= 0.3 {
							s3 = dp[i-1][j-1] + sim
						} else {
							s3 = dp[i-1][j-1] - 10.0
						}

						maxScore := s1
						if s2 > maxScore {
							maxScore = s2
						}
						if s3 > maxScore {
							maxScore = s3
						}
						dp[i][j] = maxScore
					}
				}

				// Backtrack to find alignment steps
				type alignStep struct {
					dIdx int // -1 if dummy
					iIdx int // -1 if dummy
				}
				var steps []alignStep

				i := m
				j := n
				for i > 0 || j > 0 {
					if i > 0 && j > 0 {
						sim := lineSimilarity(D[i-1].content, I[j-1].content)
						matchVal := -10.0
						if sim >= 0.3 {
							matchVal = sim
						}
						if dp[i][j] == dp[i-1][j-1]+matchVal {
							steps = append(steps, alignStep{dIdx: i - 1, iIdx: j - 1})
							i--
							j--
							continue
						}
					}
					if j > 0 && dp[i][j] == dp[i][j-1] {
						steps = append(steps, alignStep{dIdx: -1, iIdx: j - 1})
						j--
					} else if i > 0 {
						steps = append(steps, alignStep{dIdx: i - 1, iIdx: -1})
						i--
					}
				}

				// Reverse steps
				for k := 0; k < len(steps)/2; k++ {
					steps[k], steps[len(steps)-1-k] = steps[len(steps)-1-k], steps[k]
				}

				// Append aligned results
				for _, step := range steps {
					if step.dIdx != -1 && step.iIdx != -1 {
						lLine := D[step.dIdx]
						rLine := I[step.iIdx]

						leftLine := buffer.AlignedLine{
							Type:    buffer.DiffDelete,
							Content: lLine.content,
							LineNum: leftLineNum + step.dIdx,
						}
						rightLine := buffer.AlignedLine{
							Type:    buffer.DiffInsert,
							Content: rLine.content,
							LineNum: rightLineNum + step.iIdx,
						}

						lRunes := []rune(lLine.content)
						rRunes := []rune(rLine.content)

						charDiffs := d.DiffMain(string(lRunes), string(rRunes), false)

						leftLine.ChangedChars = make([]bool, len(lRunes))
						rightLine.ChangedChars = make([]bool, len(rRunes))

						lIdx := 0
						rIdx := 0
						for _, cd := range charDiffs {
							cdRunes := []rune(cd.Text)
							cdLen := len(cdRunes)

							switch cd.Type {
							case dmp.DiffEqual:
								lIdx += cdLen
								rIdx += cdLen
							case dmp.DiffDelete:
								for k := 0; k < cdLen; k++ {
									if lIdx+k < len(leftLine.ChangedChars) {
										leftLine.ChangedChars[lIdx+k] = true
									}
								}
								lIdx += cdLen
							case dmp.DiffInsert:
								for k := 0; k < cdLen; k++ {
									if rIdx+k < len(rightLine.ChangedChars) {
										rightLine.ChangedChars[rIdx+k] = true
									}
								}
								rIdx += cdLen
							}
						}

						leftResult = append(leftResult, leftLine)
						rightResult = append(rightResult, rightLine)
					} else if step.dIdx != -1 {
						leftResult = append(leftResult, buffer.AlignedLine{
							Type:    buffer.DiffDelete,
							Content: D[step.dIdx].content,
							LineNum: leftLineNum + step.dIdx,
						})
						rightResult = append(rightResult, buffer.AlignedLine{
							Type:    buffer.DiffInsert,
							Content: "",
							LineNum: 0,
						})
					} else if step.iIdx != -1 {
						leftResult = append(leftResult, buffer.AlignedLine{
							Type:    buffer.DiffDelete,
							Content: "",
							LineNum: 0,
						})
						rightResult = append(rightResult, buffer.AlignedLine{
							Type:    buffer.DiffInsert,
							Content: I[step.iIdx].content,
							LineNum: rightLineNum + step.iIdx,
						})
					}
				}

				leftLineNum += m
				rightLineNum += n
			} else {
				// Just deletes
				dCount := len(block.lines)
				for j, line := range block.lines {
					leftResult = append(leftResult, buffer.AlignedLine{
						Type:    buffer.DiffDelete,
						Content: line.content,
						LineNum: leftLineNum + j,
					})
					rightResult = append(rightResult, buffer.AlignedLine{
						Type:    buffer.DiffInsert,
						Content: "",
						LineNum: 0,
					})
				}
				leftLineNum += dCount
			}
		} else if block.op == dmp.DiffInsert {
			// This is an insert block with NO preceding delete block!
			iCount := len(block.lines)
			for j, line := range block.lines {
				leftResult = append(leftResult, buffer.AlignedLine{
					Type:    buffer.DiffDelete,
					Content: "",
					LineNum: 0,
				})
				rightResult = append(rightResult, buffer.AlignedLine{
					Type:    buffer.DiffInsert,
					Content: line.content,
					LineNum: rightLineNum + j,
				})
			}
			rightLineNum += iCount
		}
	}

	return leftResult, rightResult
}

func lcsLength(s1, s2 string) int {
	r1 := []rune(s1)
	r2 := []rune(s2)
	m := len(r1)
	n := len(r2)
	if m == 0 || n == 0 {
		return 0
	}
	dp := make([]int, n+1)
	for i := 1; i <= m; i++ {
		prev := 0
		for j := 1; j <= n; j++ {
			temp := dp[j]
			if r1[i-1] == r2[j-1] {
				dp[j] = prev + 1
			} else {
				if dp[j-1] > dp[j] {
					dp[j] = dp[j-1]
				}
			}
			prev = temp
		}
	}
	return dp[n]
}

func lineSimilarity(s1, s2 string) float64 {
	if s1 == "" && s2 == "" {
		return 1.0
	}
	t1 := strings.TrimSpace(s1)
	t2 := strings.TrimSpace(s2)
	if t1 == "" || t2 == "" {
		return 0.0
	}
	if t1 == t2 {
		return 1.0
	}
	common := lcsLength(t1, t2)
	maxLen := len(t1)
	if len(t2) > maxLen {
		maxLen = len(t2)
	}
	return float64(common) / float64(maxLen)
}
