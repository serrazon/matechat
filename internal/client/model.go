// Package client implements the matechat TUI using bubbletea.
package client

import (
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"matechat/internal/peer"
	"matechat/internal/proto"
	"matechat/internal/store"
)

// ── Color palette ────────────────────────────────────────────────────────────

const (
	clrBright    = lipgloss.Color("#00FF41") // phosphor bright green
	clrMid       = lipgloss.Color("#00CC33") // mid green
	clrBorderDim = lipgloss.Color("#005C1A") // dark green panel borders
	clrBorderHi  = lipgloss.Color("#00AA44") // medium green — self bubble
	clrBody      = lipgloss.Color("#AAFFCC") // light mint — message body
	clrDim       = lipgloss.Color("#003D0F") // very dark — timestamps
	clrSystem    = lipgloss.Color("#00DDAA") // cyan-green — system notices
	clrSelf      = lipgloss.Color("#39FF14") // neon lime — own name
	clrDirect    = lipgloss.Color("#00FF41") // bright green — direct badge
	clrHolepunch = lipgloss.Color("#00CCFF") // cyan — holepunch badge
	clrRelay     = lipgloss.Color("#FFAA00") // amber — relay badge
	clrError     = lipgloss.Color("#FF4444") // red — errors
)

var peerColors = []lipgloss.Color{
	"#00FF41", "#39FF14", "#00FFAA", "#7FFF00",
	"#00FF7F", "#ADFF2F", "#66FF66", "#00E5FF",
}

// ── Message types for the tea loop ──────────────────────────────────────────

type incomingMsg struct {
	msg proto.ChatMsg
}

type peerJoinMsg struct {
	name string
}

type peerLeaveMsg struct {
	name string
}

type historyLoadedMsg struct {
	messages []displayMsg
}

type shellResultMsg struct {
	cmd    string
	output string
}

// FileReceivedMsg is sent on fileCh when a peer completes sending a file.
type FileReceivedMsg struct {
	From     string
	Filename string
	Path     string
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ── displayMsg ───────────────────────────────────────────────────────────────

type displayMsg struct {
	From   string
	Body   string
	TS     int64 // unix milliseconds
	System bool  // true for system notices / shell output
	Shell  bool  // true for "$ cmd" header lines
}

// ── Model ────────────────────────────────────────────────────────────────────

// Model is the bubbletea model for the matechat TUI.
type Model struct {
	manager  *peer.Manager
	store    *store.Store
	messages []displayMsg
	peers    []peer.PeerStatus
	version  string

	input    textinput.Model
	viewport viewport.Model
	spinner  spinner.Model

	width  int
	height int
	ready  bool
	blink  bool

	msgChan chan proto.ChatMsg
	joinCh  chan string
	leaveCh chan string
	fileCh  chan FileReceivedMsg
	shellCh chan shellResultMsg
}

// New creates a new Model with pre-created channels for manager communication.
func New(manager *peer.Manager, st *store.Store,
	msgCh chan proto.ChatMsg, joinCh, leaveCh chan string,
	fileCh chan FileReceivedMsg, version string) Model {
	ti := textinput.New()
	ti.Placeholder = ""
	ti.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(clrBright)

	return Model{
		manager: manager,
		store:   st,
		input:   ti,
		spinner: sp,
		msgChan: msgCh,
		joinCh:  joinCh,
		leaveCh: leaveCh,
		fileCh:  fileCh,
		shellCh: make(chan shellResultMsg, 8),
		version: version,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.spinner.Tick,
		tickCmd(),
		m.waitForMessage(),
		m.waitForPeerJoin(),
		m.waitForPeerLeave(),
		m.waitForFile(),
		m.waitForShell(),
		m.loadHistory(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tickMsg:
		m.blink = !m.blink
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
		}
		return m, tickCmd()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		vpH := m.vpHeight()
		vpW := m.vpWidth()

		if !m.ready {
			m.viewport = viewport.New(vpW, vpH)
			m.viewport.SetContent(m.renderMessages())
			m.ready = true
		} else {
			m.viewport.Width = vpW
			m.viewport.Height = vpH
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.manager.Shutdown()
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			if strings.HasPrefix(text, "/") {
				return m.handleCommand(text)
			}
			if strings.HasPrefix(text, "!") {
				return m.runShell(text[1:])
			}
			m.messages = append(m.messages, displayMsg{
				From: m.manager.SelfName(),
				Body: text,
				TS:   time.Now().UnixMilli(),
			})
			if m.ready {
				m.viewport.SetContent(m.renderMessages())
				m.viewport.GotoBottom()
			}
			go m.manager.Broadcast(text)
			return m, nil
		}

	case incomingMsg:
		m.messages = append(m.messages, displayMsg{
			From: msg.msg.From,
			Body: msg.msg.Body,
			TS:   msg.msg.TS,
		})
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		cmds = append(cmds, m.waitForMessage())

	case peerJoinMsg:
		statuses := m.manager.OnlinePeerStatuses()
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		m.peers = statuses
		cmds = append(cmds, m.waitForPeerJoin())

	case peerLeaveMsg:
		statuses := m.manager.OnlinePeerStatuses()
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		m.peers = statuses
		cmds = append(cmds, m.waitForPeerLeave())

	case historyLoadedMsg:
		m.messages = msg.messages
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}

	case FileReceivedMsg:
		home, _ := os.UserHomeDir()
		displayPath := msg.Path
		if strings.HasPrefix(msg.Path, home) {
			displayPath = "~" + msg.Path[len(home):]
		}
		m.messages = append(m.messages, displayMsg{
			Body:   fmt.Sprintf("%s sent %s → saved to %s", msg.From, msg.Filename, displayPath),
			System: true,
			TS:     time.Now().UnixMilli(),
		})
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		cmds = append(cmds, m.waitForFile())

	case shellResultMsg:
		m.messages = append(m.messages, displayMsg{Body: msg.cmd, Shell: true, System: true, TS: time.Now().UnixMilli()})
		if msg.output != "" {
			m.messages = append(m.messages, displayMsg{Body: msg.output, System: true, TS: time.Now().UnixMilli()})
		}
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		cmds = append(cmds, m.waitForShell())
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	if m.ready {
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// ── Layout helpers ───────────────────────────────────────────────────────────

func (m Model) innerWidth() int {
	w := m.width - 2 // subtract two ║ border chars
	if w < 1 {
		w = 1
	}
	return w
}

func (m Model) vpHeight() int {
	h := m.height - 6 // headerHeight(3) + inputHeight(3)
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) vpWidth() int {
	return m.innerWidth()
}

// ── View ─────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if !m.ready {
		return m.renderConnecting()
	}

	header := m.renderHeader()
	body := m.renderViewport()
	inputBar := m.renderInputBar()

	return header + body + inputBar
}

func (m Model) renderConnecting() string {
	return lipgloss.NewStyle().
		Foreground(clrBright).
		Render(m.spinner.View() + " ESTABLISHING LINK...")
}

// ── Header ───────────────────────────────────────────────────────────────────

func (m Model) renderHeader() string {
	inner := m.innerWidth()

	// ── Line 1: top border with title, clock, peer count ─────────────────
	title := lipgloss.NewStyle().Bold(true).Foreground(clrBright).
		Render("MATECHAT " + m.version)
	clock := lipgloss.NewStyle().Foreground(clrMid).
		Render(time.Now().Format("15:04:05"))
	peerCnt := lipgloss.NewStyle().Foreground(clrMid).
		Render(fmt.Sprintf("%d PEERS", len(m.peers)))

	titleW := lipgloss.Width(title)
	clockW := lipgloss.Width(clock)
	peerCntW := lipgloss.Width(peerCnt)

	// ╔═ title ════ clock ═ peerCnt ═╗
	// fixed chars: "╔═ " + " ════ " + " ═ " + " ═╗" = 3 + 6 + 3 + 3 = 15
	fillCount := inner - titleW - clockW - peerCntW - 15
	if fillCount < 1 {
		fillCount = 1
	}
	fill := strings.Repeat("═", fillCount)

	dimBorder := lipgloss.NewStyle().Foreground(clrBorderDim)
	line1 := dimBorder.Render("╔═ ") + title +
		dimBorder.Render(" ════ ") + clock +
		dimBorder.Render(" ═ ") + peerCnt +
		dimBorder.Render(" ═"+fill+"╗")

	// ── Line 2: peer row ─────────────────────────────────────────────────
	selfLabel := lipgloss.NewStyle().Bold(true).Foreground(clrSelf).Render("◉ " + m.manager.SelfName())
	row := "  " + selfLabel

	for _, ps := range m.peers {
		dot := "●"
		if m.blink {
			dot = "○"
		}
		dotStr := lipgloss.NewStyle().Foreground(peerNameColor(ps.Name)).Render(dot + " " + ps.Name)
		badge := connBadge(ps.ConnType)
		row += "   " + dotStr + " " + badge
	}

	rowW := lipgloss.Width(row)
	rowPad := inner - rowW
	if rowPad < 0 {
		rowPad = 0
	}
	line2 := dimBorder.Render("║") + row + strings.Repeat(" ", rowPad) + dimBorder.Render("║")

	// ── Line 3: divider ──────────────────────────────────────────────────
	line3 := dimBorder.Render("╠" + strings.Repeat("═", inner) + "╣")

	return line1 + "\n" + line2 + "\n" + line3 + "\n"
}

func connBadge(connType string) string {
	switch connType {
	case "direct":
		return lipgloss.NewStyle().Foreground(clrDirect).Render("[DIRECT]")
	case "holepunch":
		return lipgloss.NewStyle().Foreground(clrHolepunch).Render("[HOP]")
	case "relay":
		return lipgloss.NewStyle().Foreground(clrRelay).Render("[RELAY]")
	default:
		return lipgloss.NewStyle().Foreground(clrDim).Render("[?]")
	}
}

// ── Viewport wrapper ─────────────────────────────────────────────────────────

func (m Model) renderViewport() string {
	inner := m.innerWidth()
	vpH := m.vpHeight()
	dimBorder := lipgloss.NewStyle().Foreground(clrBorderDim)

	raw := strings.TrimRight(m.viewport.View(), "\n")
	lines := strings.Split(raw, "\n")

	var sb strings.Builder
	for _, line := range lines {
		lineW := lipgloss.Width(line)
		pad := inner - lineW
		if pad < 0 {
			pad = 0
		}
		sb.WriteString(dimBorder.Render("║") + line + strings.Repeat(" ", pad) + dimBorder.Render("║") + "\n")
	}
	// pad remaining rows
	for i := len(lines); i < vpH; i++ {
		sb.WriteString(dimBorder.Render("║") + strings.Repeat(" ", inner) + dimBorder.Render("║") + "\n")
	}
	return sb.String()
}

// ── Input bar ────────────────────────────────────────────────────────────────

func (m Model) renderInputBar() string {
	inner := m.innerWidth()
	dimBorder := lipgloss.NewStyle().Foreground(clrBorderDim)

	divider := dimBorder.Render("╠" + strings.Repeat("═", inner) + "╣")

	prompt := lipgloss.NewStyle().Bold(true).Foreground(clrBright).Render("▶ TRANSMIT › ")
	promptW := lipgloss.Width(prompt)
	inputView := m.input.View()
	inputW := lipgloss.Width(inputView)
	usedW := promptW + inputW
	pad := inner - usedW
	if pad < 0 {
		pad = 0
	}
	inputLine := dimBorder.Render("║") + prompt + inputView + strings.Repeat(" ", pad) + dimBorder.Render("║")

	bottom := dimBorder.Render("╚" + strings.Repeat("═", inner) + "╝")

	return divider + "\n" + inputLine + "\n" + bottom
}

// ── Message renderer ─────────────────────────────────────────────────────────

func (m Model) renderMessages() string {
	if len(m.messages) == 0 {
		return lipgloss.NewStyle().Foreground(clrDim).Italic(true).
			Render("  no messages yet — say something!")
	}

	vpW := m.vpWidth()
	bubbleW := vpW * 3 / 4
	if bubbleW < 20 {
		bubbleW = 20
	}

	selfName := m.manager.SelfName()
	dimBorder := lipgloss.NewStyle().Foreground(clrBorderDim)
	hiBorder := lipgloss.NewStyle().Foreground(clrBorderHi)

	var sb strings.Builder
	for _, msg := range m.messages {
		if msg.System && msg.Shell {
			// Shell command header: "$ cmd"
			dollar := lipgloss.NewStyle().Foreground(clrBright).Render("$")
			sb.WriteString("  " + dollar + " " +
				lipgloss.NewStyle().Foreground(clrSystem).Render(msg.Body) + "\n")
			continue
		}
		if msg.System {
			sb.WriteString("  " +
				lipgloss.NewStyle().Foreground(clrSystem).Italic(true).Render("▸ "+msg.Body) + "\n")
			continue
		}

		isSelf := msg.From == selfName
		bStyle := dimBorder
		nameClr := peerNameColor(msg.From)
		if isSelf {
			bStyle = hiBorder
			nameClr = clrSelf
		}

		innerW := bubbleW - 2 // inside the │ chars
		nameTag := "[" + msg.From + "]"
		tsStr := ""
		if msg.TS > 0 {
			tsStr = time.UnixMilli(msg.TS).Format("15:04:05")
		}

		// Top border: ┌─[name]─────── ts ─┐
		topFill := innerW - utf8.RuneCountInString(nameTag) - utf8.RuneCountInString(tsStr) - 4
		if topFill < 1 {
			topFill = 1
		}

		nameRendered := lipgloss.NewStyle().Bold(true).Foreground(nameClr).Render(nameTag)
		tsRendered := lipgloss.NewStyle().Foreground(clrDim).Render(tsStr)

		topLine := bStyle.Render("┌─") + nameRendered +
			bStyle.Render(strings.Repeat("─", topFill)) + tsRendered +
			bStyle.Render(" ─┐")

		// Body lines (word-wrapped)
		wrapped := wordWrap(msg.Body, innerW-2)
		var bodyLines []string
		for _, line := range wrapped {
			padded := line
			lineRunes := utf8.RuneCountInString(line)
			if lineRunes < innerW-2 {
				padded += strings.Repeat(" ", innerW-2-lineRunes)
			}
			bodyLines = append(bodyLines, bStyle.Render("│ ")+
				lipgloss.NewStyle().Foreground(clrBody).Render(padded)+
				bStyle.Render(" │"))
		}

		bottomLine := bStyle.Render("└" + strings.Repeat("─", innerW) + "┘")

		// Indent: right-align self, left-align others
		indent := "  "
		if isSelf {
			leftPad := vpW - bubbleW
			if leftPad < 0 {
				leftPad = 0
			}
			indent = strings.Repeat(" ", leftPad)
		}

		sb.WriteString(indent + topLine + "\n")
		for _, bl := range bodyLines {
			sb.WriteString(indent + bl + "\n")
		}
		sb.WriteString(indent + bottomLine + "\n")
	}
	return sb.String()
}

// wordWrap splits text into lines of at most maxW runes.
func wordWrap(text string, maxW int) []string {
	if maxW < 1 {
		maxW = 1
	}
	var lines []string
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	line := ""
	for _, w := range words {
		// Hard-break words wider than maxW
		for utf8.RuneCountInString(w) > maxW {
			runes := []rune(w)
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, string(runes[:maxW]))
			w = string(runes[maxW:])
		}
		if w == "" {
			continue
		}
		if line == "" {
			line = w
		} else if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) <= maxW {
			line += " " + w
		} else {
			lines = append(lines, line)
			line = w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

// ── Commands ─────────────────────────────────────────────────────────────────

func (m Model) handleCommand(text string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])

	sysMsg := func(body string) (tea.Model, tea.Cmd) {
		m.messages = append(m.messages, displayMsg{Body: body, System: true, TS: time.Now().UnixMilli()})
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		return m, nil
	}

	switch cmd {
	case "/help":
		return sysMsg("commands: /help  /who  /clear  /send <file>  /quit")

	case "/who":
		statuses := m.manager.OnlinePeerStatuses()
		if len(statuses) == 0 {
			return sysMsg("no other peers online")
		}
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
		parts := make([]string, len(statuses))
		for i, ps := range statuses {
			parts[i] = ps.Name + " (" + ps.ConnType + ")"
		}
		return sysMsg("online: " + strings.Join(parts, ", "))

	case "/clear":
		m.messages = nil
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		return m, nil

	case "/send":
		if len(parts) < 2 {
			return sysMsg("usage: /send <filepath>")
		}
		path := strings.Join(parts[1:], " ")
		if strings.HasPrefix(path, "~/") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, path[2:])
		}
		if _, err := os.Stat(path); err != nil {
			return sysMsg("file not found: " + path)
		}
		filename := filepath.Base(path)
		m.messages = append(m.messages, displayMsg{
			Body:   "sending " + filename + "...",
			System: true,
			TS:     time.Now().UnixMilli(),
		})
		if m.ready {
			m.viewport.SetContent(m.renderMessages())
			m.viewport.GotoBottom()
		}
		go func() {
			if err := m.manager.SendFile(path); err != nil {
				log.Printf("send file: %v", err)
			}
		}()
		return m, nil

	case "/quit":
		m.manager.Shutdown()
		return m, tea.Quit

	default:
		return sysMsg("unknown command: " + cmd + " — type /help for a list")
	}
}

// ── Channel waiters ───────────────────────────────────────────────────────────

func (m Model) waitForMessage() tea.Cmd {
	return func() tea.Msg {
		msg := <-m.msgChan
		return incomingMsg{msg: msg}
	}
}

func (m Model) waitForPeerJoin() tea.Cmd {
	return func() tea.Msg {
		name := <-m.joinCh
		return peerJoinMsg{name: name}
	}
}

func (m Model) waitForPeerLeave() tea.Cmd {
	return func() tea.Msg {
		name := <-m.leaveCh
		return peerLeaveMsg{name: name}
	}
}

func (m Model) waitForFile() tea.Cmd {
	return func() tea.Msg {
		return <-m.fileCh
	}
}

func (m Model) runShell(cmdStr string) (tea.Model, tea.Cmd) {
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return m, nil
	}
	ch := m.shellCh
	go func() {
		c := exec.Command("sh", "-c", cmdStr)
		c.Dir, _ = os.UserHomeDir()
		raw, err := c.CombinedOutput()

		out := strings.TrimRight(string(raw), "\n")
		if err != nil && out == "" {
			out = err.Error()
		}

		lines := strings.Split(out, "\n")
		const maxLines = 50
		if len(lines) > maxLines {
			lines = append(lines[:maxLines], fmt.Sprintf("... (%d more lines)", len(lines)-maxLines))
		}
		out = strings.Join(lines, "\n")

		ch <- shellResultMsg{cmd: cmdStr, output: out}
	}()
	return m, nil
}

func (m Model) waitForShell() tea.Cmd {
	return func() tea.Msg {
		return <-m.shellCh
	}
}

func (m Model) loadHistory() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.store.RecentMessages(100)
		if err != nil {
			return historyLoadedMsg{}
		}
		display := make([]displayMsg, len(msgs))
		for i, msg := range msgs {
			display[i] = displayMsg{From: msg.From, Body: msg.Body, TS: msg.TS}
		}
		return historyLoadedMsg{messages: display}
	}
}

// ── Color helpers ─────────────────────────────────────────────────────────────

func peerNameColor(name string) lipgloss.Color {
	h := fnv.New32a()
	h.Write([]byte(name))
	return peerColors[int(h.Sum32())%len(peerColors)]
}
